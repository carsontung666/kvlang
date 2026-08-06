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
		key, err := resolveWriteSlot(f.KV, keytree.FrameRoot(f.PC), f.Inst.Writes[0].Name)
		if err != nil {
			return denyWrite(f, err)
		}
		if err := kvSet(f, []kvspace.KVPair{{key, result}}); err != nil {
			return err
		}
	}
	vthread.Set(bg, f.KV, f.Vtid, rwir.NextPC(f.PC), "running")
	return nil
}

// denyWrite 把落点校验失败变成 vthread 的错误终态。
//
// 只 return error 是不够的：cmd/kvlang/run.go 的 reportRunError 靠 ‥error/msg
// 决定退出码，不写它就会变成"拒绝了，但进程 exit 0"——攻击被挡住却看不出来。
func denyWrite(f *rwir.Frame, err error) error {
	vthread.SetError(bg, f.KV, f.Vtid, f.PC, err.Error())
	return err
}

// kvSet 执行写入，并把 kvspace 的失败也变成 vthread 的错误终态。
//
// 本包历来直接丢弃 kv.Set 的返回值（12 个写入点里只有 2 个接），于是 kvspace
// 的拒绝（值无法无损编解码、目录值类型不符、后端不可达…）表现为"什么都没发生
// 且 exit 0"。落点校验加进来之后，这类静默失败会掩盖校验自己的 bug ——
// 分不清"被拒了"和"写了但没落地"。
//
// 注意：写落点校验已经挡掉了非规范路径与受保护域，因此目前很难从 kvlang 源码
// 构造出一条"校验放行但 Set 失败"的路径（art 后端对标量下建子键、深层缺失父目录
// 都照收）。这里是纵深防御，不是在修一个已复现的 bug。
func kvSet(f *rwir.Frame, pairs []kvspace.KVPair) error {
	if err := f.KV.Set(pairs); err != nil {
		msg := "RuntimeError: 写入失败: " + err.Error()
		vthread.SetError(bg, f.KV, f.Vtid, f.PC, msg)
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// ResolveWriteSlot 返回写槽的绝对 KV key，供 VM 之外的写入方（rwir/dispatch 的
// 委托任务 outputs）复用。委托必须与原生写走同一条解析路径，否则同一个写槽
// 在两条路上会落到不同的键 —— 尤其是 ‥wparam 零拷贝重定向，以及本函数末尾的
// 落点校验：委托的 outputs 一旦出了门，写就发生在 VM 之外，拦不住了。
func ResolveWriteSlot(kv kvspace.KVSpace, framePath, name string) (string, error) {
	return resolveWriteSlot(kv, framePath, name)
}

// resolveWriteSlot 返回写槽的绝对 KV key（先查 ‥wparam 重定向，再处理绝对路径），
// 并校验最终落点。
//
// 校验放在**所有变换之后**：‥wparam 重定向的目标是从 KV 读出来的任意路径，
// 名字看着人畜无害不代表落点安全。
func resolveWriteSlot(kv kvspace.KVSpace, framePath, name string) (string, error) {
	key := name
	if !isAbsolute(name) {
		rwRoot := funcFrameRoot(kv, framePath)
		if r := kvspace.GetOne(kv, keytree.WParam(rwRoot, name)); !kvspace.IsNone(r) {
			key = r.String()
		} else {
			key = keytree.Stack(rwRoot) + name
		}
	}
	return key, keytree.CheckWriteKey(keytree.VtidFromPC(framePath), key)
}

// nextPC advances PC without writing a result.
func nextPC(f *rwir.Frame) {
	vthread.Set(bg, f.KV, f.Vtid, rwir.NextPC(f.PC), "running")
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
	// 两趟：先解析并校验**全部**写槽，再统一写入。
	//
	// 逐槽校验是必须的（copy 可以有多个写槽，"SRC" -> ok, /lib/x，校验提到循环外
	// 就漏掉第 2..N 槽）；但"边校验边写"同样不行 —— 第 2 槽被拒时第 1 槽已经落盘，
	// 拒绝就成了半执行。实测过：`"SRC" -> ok, /lib/slot2` 报 PermissionError 的同时
	// ok 已经等于 "SRC"。dictOp 本来就是先全校验再一次 Set，这里对齐它。
	pairs := make([]kvspace.KVPair, 0, len(inst.Writes))
	for _, w := range inst.Writes {
		key, err := resolveWriteSlot(kv, framePath, w.Name)
		if err != nil {
			vthread.SetError(bg, kv, vtid, pc, err.Error())
			return err
		}
		pairs = append(pairs, kvspace.KVPair{key, v})
	}
	if len(pairs) > 0 {
		if err := kv.Set(pairs); err != nil {
			msg := "RuntimeError: 写入失败: " + err.Error()
			vthread.SetError(bg, kv, vtid, pc, msg)
			return fmt.Errorf("%s", msg)
		}
	}
	vthread.Set(bg, kv, vtid, rwir.NextPC(pc), "running")
	return nil
}
