package builtin

import (
	"kvlang/keytree"
	"github.com/array2d/kvspace-go"
	"kvlang/rwir"
)

func isAbsolute(param string) bool { return len(param) > 0 && param[0] == '/' }

// writeSlotKey 返回 slot 在 rwfunc 帧的绝对 KV key。
func writeSlotKey(kv kvspace.KVSpace, framePath, slot string) string {
	if isAbsolute(slot) { return slot }
	return keytree.Stack(funcFrameRoot(kv, framePath)) + slot
}

// ResolveReadValue maps a read-slot param to a typed Value.
func ResolveReadValue(kv kvspace.KVSpace, framePath string, param rwir.Param) kvspace.XValue {
	return resolveReadValue(kv, framePath, param)
}

// ResolveReadKey 返回读槽的绝对 KV key —— 与 ResolveWriteKey 对称，导出给
// rwir/dispatch 用，使委托任务里的输入引用是外部执行器真能读的路径。
// 字面量没有对应的键，返回 ""。
func ResolveReadKey(kv kvspace.KVSpace, framePath string, param rwir.Param) string {
	if !kvspace.IsNone(param.Val) && param.Val.Kind() != kvspace.KindRwir && param.Val.Kind() != kvspace.KindRwfunc {
		return "" // 字面量
	}
	name := param.Name
	if len(name) == 0 {
		return ""
	}
	if isAbsolute(name) {
		return name
	}
	rwRoot := funcFrameRoot(kv, framePath)
	if r := kvspace.GetOne(kv, keytree.RParam(rwRoot, name)); !kvspace.IsNone(r) {
		return r.String() // 读参零拷贝重定向
	}
	return keytree.Stack(rwRoot) + name
}

// resolveReadValue 从 rwfunc 帧查读参值。
// 字面量（Kind ≠ rwir/rwfunc）→ 直接返 Val。变量引用 → 帧查找。
func resolveReadValue(kv kvspace.KVSpace, framePath string, param rwir.Param) kvspace.XValue {
	if !kvspace.IsNone(param.Val) && param.Val.Kind() != kvspace.KindRwir && param.Val.Kind() != kvspace.KindRwfunc {
		return param.Val
	}
	name := param.Name
	if len(name) == 0 {
		return kvspace.None{}
	}
	if isAbsolute(name) {
		return kvspace.GetOne(kv, name)
	}
	rwRoot := funcFrameRoot(kv, framePath)
	if r := kvspace.GetOne(kv, keytree.RParam(rwRoot, name)); !kvspace.IsNone(r) {
		return kvspace.GetOne(kv, r.String())
	}
	if v := kvspace.GetOne(kv, keytree.Stack(rwRoot)+name); !kvspace.IsNone(v) {
		return v
	}
	return kvspace.None{}
}
