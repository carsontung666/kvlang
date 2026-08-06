package builtin

import (
	"kvlang/keytree"
	"github.com/array2d/kvspace-go"
	"kvlang/rwir"
	"kvlang/vthread"
)

func init() {
	Register("dict", "", dictOp{})
}

// dictOp: dict(k1, v1, k2, v2, ...) -> base —— dict 字面量 { k1=v1; k2=v2 } 的运行时。
// base 键写入 kind="dict" 类型标记，成员写入平坦键族 base.k（keytree.Member）。
// 值为 null（如 null 裸名解析结果）时跳过写入——kvspace 中缺席即 null。
type dictOp struct{}
func (dictOp) Call(f *rwir.Frame) error {
	inputs := readInputs(f)
	fp := keytree.FrameRoot(f.PC)
	var pairs []kvspace.KVPair
	for _, w := range f.Inst.Writes {
		outKey, err := writeSlotKey(f.KV, fp, w.Name)
		if err != nil { return denyWrite(f, err) }
		pairs = append(pairs, kvspace.KVPair{outKey, kvspace.Dict{}})
		for i := 0; i+1 < len(inputs); i += 2 {
			if kvspace.IsNone(inputs[i+1]) { continue }
			// 成员键由运行期字符串拼出，必须再校验一次：outKey 安全不代表
			// outKey+"."+k 安全，k 里可以带 /。
			mk := keytree.Member(outKey, inputs[i].String())
			if err := checkMemberKey(fp, mk); err != nil { return denyWrite(f, err) }
			pairs = append(pairs, kvspace.KVPair{mk, inputs[i+1]})
		}
	}
	if len(pairs) > 0 {
		if err := kvSet(f, pairs); err != nil { return err }
	}
	vthread.Set(bg, f.KV, f.Vtid, rwir.NextPC(f.PC), "running")
	return nil
}
