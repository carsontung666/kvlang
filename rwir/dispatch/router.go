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
	opname := stripVTypePrefix(opcode)

	// 命名空间优先：llm.chat 先找 backend "llm"。
	// 否则 Select 会扫所有 backend 找第一个注册了 "chat" 的，llm.chat 与 db.chat
	// 就会互相抢——一有第二个 provider 必然踩。
	if ns := opNamespace(opcode); ns != "" {
		if v := kvspace.GetOne(kv, keytree.SysOpFunc(ns, opname)); !kvspace.IsNone(v) {
			backend = ns
		}
	}

	// 回退：扫全部 backend。List 对目录子项返回带尾斜杠的名字（叶子不带），
	// /sys/op/<backend> 必为目录，不剥离则拼出 /sys/op/<b>//func/<op>，
	// kvspace 对双斜杠路径直接 panic。
	backends := kv.List(keytree.SysOpRoot+keytree.PathSegSep, false)
	for _, b := range backends {
		if backend != "" {
			break
		}
		b = strings.TrimSuffix(b, keytree.PathSegSep)
		if v := kvspace.GetOne(kv, keytree.SysOpFunc(b, opname)); !kvspace.IsNone(v) {
			backend = b
		}
	}
	if backend == "" {
		return "", "", fmt.Errorf("no backend supports opcode=%s", opcode)
	}

	children := kv.List(keytree.SysOpRoot + keytree.PathSegSep + backend + keytree.PathSegSep, false)

	bestLoad := math.MaxFloat64
	for _, child := range children {
		child = strings.TrimSuffix(child, keytree.PathSegSep)
		if child == "func" {
			continue // 跳过 /sys/op/<backend>/func/ 子树（List 返回 "func/"，须先剥尾斜杠）
		}
		val := kvspace.GetOne(kv, keytree.SysOp(backend, child))
		if kvspace.IsNone(val) {
			continue
		}
		var info instInfo
		if json.Unmarshal([]byte(val.String()), &info) != nil {
			logx.Debug("Select: unmarshal %s/%s: invalid", backend, child)
			continue
		}
		if info.Status != "running" {
			continue
		}
		if info.Load < bestLoad {
			bestLoad = info.Load
			n = child
		}
	}

	if n == "" {
		return "", "", fmt.Errorf("no running instance for backend=%s", backend)
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

// ListBackends 返回所有已注册 backend 名称（已剥尾斜杠，与 Select 一致）。
func ListBackends(ctx context.Context, kv kvspace.KVSpace) ([]string, error) {
	raw := kv.List(keytree.SysOpRoot+keytree.PathSegSep, false)
	names := make([]string, 0, len(raw))
	for _, b := range raw {
		names = append(names, strings.TrimSuffix(b, keytree.PathSegSep))
	}
	return names, nil
}

// BackendSupports 返回 backend 是否支持某 opcode。
func BackendSupports(ctx context.Context, kv kvspace.KVSpace, backend, opcode string) bool {
	return !kvspace.IsNone(kvspace.GetOne(kv, keytree.SysOpFunc(backend, stripVTypePrefix(opcode))))
}

// opNamespace 返回 opcode 的命名空间段（最后一个点之前的部分）。
// "llm.chat" → "llm"；"a.b.c" → "a.b"；无点则返回 ""。
//
// 必须与 keytree.FuncKey 用同一个切分规则（LastIndex），否则同一个 opcode
// 在「查签名」和「选后端」两处会被拆成不同的名字。
func opNamespace(opcode string) string {
	if dot := strings.LastIndex(opcode, keytree.MemberSep); dot > 0 {
		return opcode[:dot]
	}
	return ""
}

// stripVTypePrefix 剥离命名空间前缀，返回注册名。
// "llm.chat" → "chat"；"a.b.c" → "c"；无前缀则原样返回。
func stripVTypePrefix(opcode string) string {
	if dot := strings.LastIndex(opcode, keytree.MemberSep); dot > 0 {
		return opcode[dot+len(keytree.MemberSep):]
	}
	return opcode
}
