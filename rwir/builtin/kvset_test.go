package builtin_test

// kvSet 新增的失败路径的测试。
//
// 为什么要注入一个会失败的后端：写落点校验已经挡掉了非规范路径与受保护域，
// 因此现在很难从 kvlang 源码构造出一条"校验放行但 kvspace.Set 失败"的输入
// （art 后端对标量下建子键、深层缺失父目录都照收）。但这条路径在类型上是可达的
// ——后端不可达、值无法无损编解码都会走到它——而它以前是被**整个丢弃**的：
// rwir/builtin 的 12 个 kv.Set 里只有 2 个接了返回值，其余的失败表现为
// "什么都没发生且 exit 0"。这种静默失败最大的害处是会掩盖写落点校验自己的 bug：
// 分不清"被拒了"和"写了但没落地"。所以这里用一个只让 Set 失败的后端来钉住它。

import (
	"errors"
	"strings"
	"testing"

	"github.com/array2d/kvspace-go"
	_ "github.com/array2d/kvspace-go/art"

	"kvlang/keytree"
	"kvlang/rwir"
	"kvlang/rwir/builtin"
	"kvlang/vthread"
)

// failingSet 内嵌一个真实的 art 后端（读、目录操作都照常），只把 Set 换成必然失败。
type failingSet struct {
	kvspace.KVSpace
}

func (failingSet) Set(pairs []kvspace.KVPair) error {
	return errors.New("注入的写失败")
}

func TestKvSetFailureTerminatesVthread(t *testing.T) {
	real := kvspace.Conn("art://")
	real.DelTree(keytree.VthreadRoot)
	kvspace.MkIndexRecursive(real, keytree.VthreadRoot+keytree.PathSegSep)

	const vtid = "8001"
	frame := keytree.VThread(vtid)
	kvspace.MkIndexRecursive(real, frame+keytree.PathSegSep)
	pc := keytree.EntryPC(frame)

	kv := failingSet{real}
	// 合法写槽（自己 vthread 的普通变量），所以不会被落点校验拦下，
	// 一定会走到 kv.Set，然后被注入的失败击中。
	inst := &rwir.Rwir{
		Opcode: "=",
		Reads:  []rwir.Param{{Name: "src", Val: kvspace.NewChar("V")}},
		Writes: []rwir.Param{{Name: "dst"}},
	}
	err := builtin.ExecuteCopy(kv, vtid, pc, inst)

	if err == nil {
		t.Fatal("kv.Set 失败时必须返回错误，不能静默吞掉")
	}
	if !strings.Contains(err.Error(), "写入失败") {
		t.Fatalf("错误应指明是写入失败，实得 %v", err)
	}
	// 注意这里**不**断言 ‥error/msg：当失败的正是 kv.Set 本身时，
	// vthread.SetError 同样写不进去（它也要靠 Set）。这正是为什么
	// cmd/kvlang/run.go 的 executeEntry 除了读 ‥error/msg，还必须接住
	// cpu.Execute 的返回值 —— 否则这条路径就是 exit 0 的静默失败。
	// 本用例能钉住的是"错误确实被返回了"，退出码那一半由 executeEntry 负责。

	// PC 不得推进：写失败了还往下跑，等于用一个没写成的值继续算。
	gotPC, _ := vthread.Get(nil, real, vtid)
	if gotPC == rwir.NextPC(pc) {
		t.Fatalf("写失败后 PC 不该推进到 %q", gotPC)
	}

	real.DelTree(keytree.VthreadRoot)
	kvspace.MkIndexRecursive(real, keytree.VthreadRoot+keytree.PathSegSep)
}
