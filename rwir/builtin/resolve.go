package builtin

import (
	"errors"
	"strings"

	"kvlang/keytree"
	"github.com/array2d/kvspace-go"
	"kvlang/rwir"
)

func isAbsolute(param string) bool { return len(param) > 0 && param[0] == '/' }

// writeSlotKey 返回 slot 在 rwfunc 帧的绝对 KV key，并校验最终落点。
//
// 这是与 resolveWriteSlot 并行的**第二条写解析路径**（它不查 ‥wparam 重定向）。
// 两条都必须校验：只守一条，换个算子就绕过去了。
func writeSlotKey(kv kvspace.KVSpace, framePath, slot string) (string, error) {
	key := slot
	if !isAbsolute(slot) {
		key = keytree.Stack(funcFrameRoot(kv, framePath)) + slot
	}
	return key, keytree.CheckWriteKey(keytree.VtidFromPC(framePath), key)
}

// checkMemberKey 校验由运行期字符串拼出来的成员键。
//
// dict 字面量与 set 的路径分支都是 Member(base, <运行期字符串>)，而 kvKey 原样
// 返回那个字符串 —— base 校验过**不代表** base+"."+k 安全，k 里可以带 /。
// 合成之后必须再校验一次，否则这就是第 N 扇门。
//
// 错误信息要改写：CheckWriteKey 的措辞是给"写槽"用的，而这里用户根本没写写槽
// （`set("/a/", "/b", "V")` 里 /a/./b 是 base 与运行期键拼出来的）。照抄会让
// 诊断指向一个源码里不存在的概念。
func checkMemberKey(framePath, key string) error {
	err := keytree.CheckWriteKey(keytree.VtidFromPC(framePath), key)
	if err == nil {
		return nil
	}
	return errors.New(strings.Replace(err.Error(), "写槽", "成员键", 1))
}

// ResolveReadValue maps a read-slot param to a typed Value.
func ResolveReadValue(kv kvspace.KVSpace, framePath string, param rwir.Param) kvspace.XValue {
	return resolveReadValue(kv, framePath, param)
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
