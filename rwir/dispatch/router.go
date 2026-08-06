package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/array2d/kvspace-go"
	"kvlang/keytree"
	"kvlang/logx"
)

// instInfo 是后端实例在 /sys/op/<backend>/<n> 中存储的注册信息。
type instInfo struct {
	Status string  `json:"status"` // "running" | "stopped"
	Load   float64 `json:"load"`   // 负载 [0,1]
}

// Select 为 opcode 挑一个 running 的执行器实例。
// 返回 (backend, n, error)，调用方用 keytree.SysOpCmd(backend, n) 构造命令队列。
//
// 注册布局：
//
//	/sys/op/<backend>/func/<opname>   能力声明，存在即算支持（值无所谓）
//	/sys/op/<backend>/<n>             实例记录 {status, load}
//	/sys/op/<backend>/<n>/cmd         该实例的命令队列（Notify 目标）
//
// 命名空间即后端名：llm.chat 只在 backend "llm" 里找，找不到就报错，不静默
// 回退到某个碰巧也注册了 "chat" 的后端 —— 那会把任务投给不相干的 provider。
// 裸算子（无点）才扫全部后端。
func Select(ctx context.Context, kv kvspace.KVSpace, opcode string) (backend, n string, err error) {
	ns, opname := splitOp(opcode)

	var candidates []string
	if ns != "" {
		if !kvspace.IsNone(kvspace.GetOne(kv, keytree.SysOpFunc(ns, opname))) {
			candidates = []string{ns}
		}
	} else {
		for _, b := range listChildren(kv, keytree.SysOpRoot+keytree.PathSegSep) {
			if !kvspace.IsNone(kvspace.GetOne(kv, keytree.SysOpFunc(b, opname))) {
				candidates = append(candidates, b)
			}
		}
	}
	if len(candidates) == 0 {
		return "", "", fmt.Errorf("no backend supports opcode=%s", opcode)
	}

	// 在**全部候选后端的全部 running 实例**里挑负载最低的。
	// 先锁定后端再筛实例的话，首个候选恰好没有 running 实例就会硬失败，
	// 哪怕另一个候选完全可用。
	bestLoad := math.MaxFloat64
	for _, b := range candidates {
		for _, child := range listChildren(kv, keytree.SysOpRoot+keytree.PathSegSep+b+keytree.PathSegSep) {
			if child == keytree.SegFunc {
				continue // 能力声明子树，不是实例
			}
			val := kvspace.GetOne(kv, keytree.SysOp(b, child))
			if kvspace.IsNone(val) {
				continue
			}
			var info instInfo
			if json.Unmarshal([]byte(val.String()), &info) != nil {
				logx.Debug("Select: unmarshal %s/%s: invalid", b, child)
				continue
			}
			if info.Status != "running" || info.Load >= bestLoad {
				continue
			}
			bestLoad, backend, n = info.Load, b, child
		}
	}

	if n == "" {
		return "", "", fmt.Errorf("no running instance for opcode=%s (backends=%v)", opcode, candidates)
	}
	return backend, n, nil
}

// listChildren 列出目录的直接子项名，并剥掉目录项的尾斜杠。
//
// kv.List 对目录子项返回带尾斜杠的名字（叶子项不带）：/sys/op/ 下的 backend
// 全是目录，返回的是 "fake/" 而非 "fake"。不剥离就会拼出 /sys/op/fake//func/x，
// 而 kvspace 对非规范路径是 panic 而非报错。
func listChildren(kv kvspace.KVSpace, dir string) []string {
	names := kv.List(dir, false)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, strings.TrimSuffix(name, keytree.PathSegSep))
	}
	return out
}

// IsDelegated 判断 opcode 是否为委托 rwir：/lib 下签名键的 XValue kind 为 rwir
// （有签名、无指令体）即委托；kind 为 rwfunc（有指令体）则是普通用户函数。
//
// 判据是「声明」，不是命名空间前缀 —— `lib tensor { rwfunc matmul() }` 写出来
// 的是 rwfunc，不会被误判为委托，也就不需要保留任何硬编码的前缀名单。
//
// 代价：这里读一次签名键，随后 HandleCall 还会读同一个键。art:// 下是内存读，
// redis:// 下每次用户函数调用多一个 RTT。要消掉得把 sigVal 传进 HandleCall。
func IsDelegated(kv kvspace.KVSpace, opcode string) bool {
	_, ok := delegatedSig(kv, opcode)
	return ok
}

// delegatedSig 读 opcode 的 /lib 签名键，返回声明的读写参个数。
// ok=false 表示这不是一个委托 rwir（键不存在，或 kind 不是 rwir）。
func delegatedSig(kv kvspace.KVSpace, opcode string) (sig kvspace.XValue, ok bool) {
	v := kvspace.GetOne(kv, keytree.FuncKey(opcode))
	if kvspace.IsNone(v) || v.Kind() != kvspace.KindRwir {
		return nil, false
	}
	return v, true
}

// splitOp 拆分 opcode 为命名空间与注册名。**按最后一个点切分。**
//
//	"llm.chat" → ("llm","chat")
//	"a.b.c"    → ("a.b","c")      注意不是 ("a","b.c")
//	"add"      → ("","add")
//
// 切分点在这里是语义的一部分，不是实现细节：ns 直接当后端名去查
// /sys/op/<ns>/func/<name>，执行器必须按同一规则注册，否则永远匹配不上。
// 取最后一个点是为了与 /lib 的 pkg/name 视角一致 —— `rwir a.b.c` 存在
// /lib/a.b.c，其中 pkg=a.b、name=c，于是后端名就是 a.b、注册名就是 c。
//
// （keytree.FuncKey 也在拆点，但那边拆完又拼回去，拆哪个点结果都一样；
// 两处不存在"必须一致"的约束，别把它们的规则绑在一起想。）
func splitOp(opcode string) (ns, name string) {
	if dot := strings.LastIndex(opcode, keytree.MemberSep); dot > 0 {
		return opcode[:dot], opcode[dot+len(keytree.MemberSep):]
	}
	return "", opcode
}
