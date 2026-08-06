package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"kvlang/keytree"
	"github.com/array2d/kvspace-go"
	"kvlang/logx"
)

// instInfo 是后端实例在 /sys/op/<backend>/<n> 中存储的注册信息。
type instInfo struct {
	Status string  `json:"status"` // "running" | "stopped"
	Load   float64 `json:"load"`   // 负载 [0,1]
}

// Select 根据 opcode 选择负载最低的 op 实例。
// 返回 (backend, n, error)，调用方用 keytree.SysOpCmd(backend, n) 构造命令队列。
//
// 流程：
//  1. kv.List("/sys/op")               → 所有已注册 backend 名，如 ["buildin","cuda"]
//  2. /sys/op/<backend>/func/<opname>  → 筛选支持该操作的 backend
//  3. kv.List("/sys/op/<backend>")     → 子项，过滤掉 "func"，剩下实例编号 ["0","1",…]
//  4. 读取各实例 {status, load}，选负载最低的 running 实例
func Select(ctx context.Context, kv kvspace.KVSpace, opcode string) (backend, n string, err error) {
	ns, opname := splitOp(opcode)

	// 命名空间即后端名。llm.chat 只找 backend "llm"，找不到就报错 ——
	// 不静默回退到某个碰巧也注册了 "chat" 的后端（会把任务投给不相干的 provider）。
	var candidates []string
	if ns != "" {
		if !kvspace.IsNone(kvspace.GetOne(kv, keytree.SysOpFunc(ns, opname))) {
			candidates = []string{ns}
		}
	} else {
		// 无命名空间的裸算子：扫全部后端。List 对目录子项返回带尾斜杠的名字，
		// 不剥离则拼出 /sys/op/<b>//func/<op>，kvspace 对双斜杠直接 panic。
		for _, b := range kv.List(keytree.SysOpRoot+keytree.PathSegSep, false) {
			b = strings.TrimSuffix(b, keytree.PathSegSep)
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
		for _, child := range kv.List(keytree.SysOpRoot+keytree.PathSegSep+b+keytree.PathSegSep, false) {
			child = strings.TrimSuffix(child, keytree.PathSegSep)
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

// IsDelegated 判断 opcode 是否为委托 rwir：/lib 下签名键的 XValue kind 为 rwir
// （有签名、无指令体）即委托；kind 为 rwfunc（有指令体）则是普通用户函数。
//
// 判据本身就是 HandleCall 要读的那个键，所以委托与否是「声明」决定的，不是命名空间
// 前缀决定的 —— lib tensor { } 写出来的是 rwfunc，不会被误判为委托。
func IsDelegated(kv kvspace.KVSpace, opcode string) bool {
	v := kvspace.GetOne(kv, keytree.FuncKey(opcode))
	return !kvspace.IsNone(v) && v.Kind() == kvspace.KindRwir
}


// splitOp 拆分 opcode 为命名空间与注册名。
// "llm.chat" → ("llm","chat")；"a.b.c" → ("a.b","c")；无点 → ("", opcode)。
// 切分规则必须与 keytree.FuncKey 一致（LastIndex），否则同一个 opcode 在
// 「查签名」与「选后端」两处会被拆成不同的名字。
func splitOp(opcode string) (ns, name string) {
	if dot := strings.LastIndex(opcode, keytree.MemberSep); dot > 0 {
		return opcode[:dot], opcode[dot+len(keytree.MemberSep):]
	}
	return "", opcode
}
