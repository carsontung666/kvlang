package builtin

import (
	"fmt"

	"kvlang/keytree"
	"github.com/array2d/kvspace-go"
	"kvlang/rwir"
	"kvlang/vthread"
)

func requireBinary(inputs []kvspace.XValue) error {
	if len(inputs) != 2 { return fmt.Errorf("binary op requires 2 inputs, got %d", len(inputs)) }
	return nil
}
func requireUnary(inputs []kvspace.XValue) error {
	if len(inputs) != 1 { return fmt.Errorf("unary op requires 1 input, got %d", len(inputs)) }
	return nil
}

// requireNumeric guards that all inputs are numeric (int or float kinds).
func requireNumeric(inputs ...kvspace.XValue) error {
	for _, v := range inputs {
		if !isNumeric(v) {
			return fmt.Errorf("TypeError: expected numeric, got %s", v.Kind())
		}
	}
	return nil
}

// requireInt guards that all inputs are integer kinds.
func requireInt(inputs ...kvspace.XValue) error {
	for _, v := range inputs {
		if !isIntKind(v.Kind()) {
			return fmt.Errorf("TypeError: expected integer, got %s", v.Kind())
		}
	}
	return nil
}

// requireKind guards that an input has a specific kind.
func requireKind(v kvspace.XValue, kind string) error {
	if v.Kind() != kind {
		return fmt.Errorf("TypeError: expected %s, got %s", kind, v.Kind())
	}
	return nil
}

// readInputs resolves all read-slots of f.Inst into typed Values.
func readInputs(f *rwir.Frame) []kvspace.XValue {
	framePath := funcFrameRoot(f.KV, keytree.FrameRoot(f.PC))
	inputs := make([]kvspace.XValue, 0, len(f.Inst.Reads))
	for _, r := range f.Inst.Reads {
		inputs = append(inputs, resolveReadValue(f.KV, framePath, r))
	}
	return inputs
}

// writeResult writes a typed Value to the first write-slot and advances PC.
func writeResult(f *rwir.Frame, result kvspace.XValue) error {
	if len(f.Inst.Writes) > 0 {
		key, err := writeSlotChecked(f.KV, keytree.FrameRoot(f.PC), f.Inst.Writes[0].Name)
		if err != nil {
			return writeDenied(f.KV, f.Vtid, f.PC, err)
		}
		if err := f.KV.Set([]kvspace.KVPair{{key, result}}); err != nil {
			return err
		}
	}
	vthread.Set(bg, f.KV, f.Vtid, rwir.NextPC(f.PC), "running")
	return nil
}

// ResolveWriteKey 返回写槽的绝对 KV key —— 导出给 rwir/dispatch 用，
// 使委托路径与 VM 自身走完全相同的写槽解析（含 ‥wparam 零拷贝重定向）。
func ResolveWriteKey(kv kvspace.KVSpace, framePath, name string) (string, error) {
	return writeSlotChecked(kv, framePath, name)
}

// resolveWriteSlot 返回写槽的绝对 KV key（先查 .wparam 重定向，再处理绝对路径）。
func resolveWriteSlot(kv kvspace.KVSpace, framePath, name string) string {
	if isAbsolute(name) { return name }
	rwRoot := funcFrameRoot(kv, framePath)
	if r := kvspace.GetOne(kv, keytree.WParam(rwRoot, name)); !kvspace.IsNone(r) {
		return r.String()
	}
	return keytree.Stack(rwRoot) + name
}

// writeSlotChecked 解析写槽并校验落点。所有写路径（原生与委托）都必须走它，
// 否则守住一扇门等于没守——`f(x) -> tmp` 再 `tmp -> /lib/pwned` 两行即可绕过。
func writeSlotChecked(kv kvspace.KVSpace, framePath, name string) (string, error) {
	key := resolveWriteSlot(kv, framePath, name)
	return key, keytree.CheckWriteKey(keytree.VtidFromPC(framePath), key)
}

// nextPC advances PC without writing a result.
func nextPC(f *rwir.Frame) {
	vthread.Set(bg, f.KV, f.Vtid, rwir.NextPC(f.PC), "running")
}

// setWrite writes a typed Value to a named slot (先查 .wparam 重定向).
func setWrite(kv kvspace.KVSpace, framePath, slot string, val kvspace.XValue) error {
	key, err := writeSlotChecked(kv, framePath, slot)
	if err != nil {
		return err
	}
	return kv.Set([]kvspace.KVPair{{key, val}})
}

// writeDenied 把落点校验失败转成 vthread 异常终止。
func writeDenied(kv kvspace.KVSpace, vtid, pc string, err error) error {
	msg := "RuntimeError: " + err.Error()
	vthread.SetError(bg, kv, vtid, pc, msg)
	return fmt.Errorf("%s", msg)
}

// isContainerKind reports whether a kind represents a container/marker type
// (dict, index, etc.) whose String() method does not return a path.
func isContainerKind(kind string) bool {
	switch kind {
	case "dict", "index", "linkindex", "extindex", "rwfunc", "rwir":
		return true
	}
	return false
}

// resolveBasePath resolves the first read-slot of an at/has/set instruction as a KV base path.
// For container types (dict, index, etc.) and None, resolves the variable name from the function frame.
// For string values starting with "/", uses the string directly as a path.
func resolveBasePath(kv kvspace.KVSpace, fp, funcFrame string, read rwir.Param) string {
	v := resolveReadValue(kv, fp, read)
	if kvspace.IsNone(v) || isContainerKind(v.Kind()) {
		return resolveKVPath(funcFrame, read.Name)
	}
	return v.String()
}

// funcFrameRoot returns the nearest rwfunc frame root from the given frame path.
// Uses .lib marker to identify rwfunc frames (extKind fails due to extindex cascade).
func funcFrameRoot(kv kvspace.KVSpace, frameRoot string) string {
	for f := frameRoot; f != ""; f = keytree.ParentFrame(f) {
		if !kvspace.IsNone(kvspace.GetOne(kv, keytree.Stack(f)+keytree.SegLib)) {
			return f
		}
	}
	return frameRoot
}

// ExecuteCopy copies the Value addressed by inst.Reads[0] to all write-slots (先查 .wparam 重定向).
// Preserves the original type — int stays int, float stays float.
func ExecuteCopy(kv kvspace.KVSpace, vtid, pc string, inst *rwir.Rwir) error {
	framePath := keytree.FrameRoot(pc)
	if len(inst.Reads) == 0 {
		vthread.Set(bg, kv, vtid, rwir.NextPC(pc), "running")
		return nil
	}
	v := resolveReadValue(kv, framePath, inst.Reads[0])
	for _, w := range inst.Writes {
		key, err := writeSlotChecked(kv, framePath, w.Name)
		if err != nil {
			return writeDenied(kv, vtid, pc, err)
		}
		if err := kv.Set([]kvspace.KVPair{{key, v}}); err != nil {
			return err
		}
	}
	vthread.Set(bg, kv, vtid, rwir.NextPC(pc), "running")
	return nil
}
