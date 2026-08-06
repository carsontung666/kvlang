package dispatch_test

// 委托 rwir 的端到端测试：源码 → layout → 执行 → 派发给外部执行器 → 写回 → 续跑。
//
// 执行器在这里是一个 goroutine 而非独立进程（art:// 是进程内 kvspace），
// 但它只通过 kvspace 与 VM 交互，走的是与真实外部进程完全相同的协议。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/array2d/kvspace-go"
	_ "github.com/array2d/kvspace-go/art"

	"kvlang/ast"
	"kvlang/keytree"
	"kvlang/kvcpu"
	"kvlang/layout"
	"kvlang/lower"
	"kvlang/parser"
	"kvlang/rwir/builtin"
	"kvlang/rwir/dispatch"
	"kvlang/vthread"
)

// loadSrc 把源码编译进 kvspace，照搬 cmd/kvlang/layoutandrun.go 的流程：
// 顶层调用包成 /lib/init，执行入口是 init 而非某个具体函数 —— 直接 Bootstrap
// 一个普通 rwfunc 会绕过这条路径，控制流 scope 帧不会被执行。
func loadSrc(t *testing.T, kv kvspace.KVSpace, src string) {
	t.Helper()
	df, diags, err := parser.ParseCode(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parser.HasErrors(diags) {
		t.Fatalf("parse diagnostics: %v", diags)
	}
	layout.WriteDecls(kv, df)
	for i := range df.Funcs {
		layout.WriteFunc(kv, df.Funcs[i].Pkg, lower.Func(&df.Funcs[i]))
	}
	body := df.InitBody
	for _, c := range df.TopLevelCalls {
		body = append(body, c)
	}
	if len(body) > 0 {
		initFn := ast.Func{Sig: ast.FuncSig{Name: "init"}, Body: body}
		layout.WriteFunc(kv, "", lower.Func(&initFn))
	}
}

// startBackend 起一个 echo 执行器：注册能力，然后循环消费命令队列。
// 返回的 stop 关闭后 goroutine 退出。
func startBackend(t *testing.T, kv kvspace.KVSpace, name, op string) (stop func()) {
	t.Helper()
	if err := kv.Set([]kvspace.KVPair{
		{Key: keytree.SysOpFunc(name, op), Val: kvspace.NewChar("1")},
	}); err != nil {
		t.Fatalf("register %s.%s: %v", name, op, err)
	}
	if err := kv.Set([]kvspace.KVPair{
		{Key: keytree.SysOp(name, "0"), Val: kvspace.NewChar(`{"status":"running","load":0}`)},
	}); err != nil {
		t.Fatalf("register instance: %v", err)
	}

	done := make(chan struct{})
	go func() {
		queue := keytree.SysOpCmd(name, "0")
		for {
			select {
			case <-done:
				return
			default:
			}
			raw := kv.Watch(queue, 200*time.Millisecond)
			if raw.String() == "" {
				continue
			}
			var task dispatch.OpTask
			if err := json.Unmarshal([]byte(raw.String()), &task); err != nil {
				t.Errorf("backend: bad task JSON %q: %v", raw.String(), err)
				continue
			}
			// echo：把第一个输入原样写进每个输出槽
			var val string
			if len(task.Inputs) > 0 {
				val = task.Inputs[0].Value
			}
			for _, out := range task.Outputs {
				kv.Set([]kvspace.KVPair{{Key: out.Key, Val: kvspace.NewChar(val)}})
			}
			// 状态在先，信号在后 —— VM 收到信号后会复查 .status
			kv.Set([]kvspace.KVPair{
				{Key: keytree.SysTask(task.ID, "status"), Val: kvspace.NewChar("done")},
			})
			kv.Notify(task.DoneKey, kvspace.NewChar("1"))
		}
	}()
	return func() { close(done) }
}

// run 编译并执行 entry，返回 vtid。
func run(t *testing.T, kv kvspace.KVSpace, entry string) string {
	t.Helper()
	vtid := vthread.AllocVtid(kv)
	kv.DelTree(keytree.VThread(vtid))
	kvspace.MkIndexRecursive(kv, keytree.VThread(vtid)+keytree.PathSegSep)
	builtin.WriteSysRwir(kv)
	pc := layout.Bootstrap(context.Background(), kv, vtid, entry, nil)
	if pc == "" {
		t.Fatalf("Bootstrap %s failed", entry)
	}
	vthread.Set(context.Background(), kv, vtid, pc, "init")
	if err := kvcpu.New(kv, "test").Execute(pc); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return vtid
}

func newKV(t *testing.T) kvspace.KVSpace {
	t.Helper()
	kv := kvspace.Conn("art://")
	if kv == nil {
		t.Fatal("Conn(art://) 返回 nil")
	}
	t.Cleanup(func() { kv.DisConn() })
	kvspace.MkIndexRecursive(kv, keytree.LibRoot+keytree.PathSegSep)
	kvspace.MkIndexRecursive(kv, keytree.VthreadRoot+keytree.PathSegSep)
	return kv
}

// TestDelegateWriteParamRedirect 是核心用例：委托指令出现在嵌套 rwfunc 帧内时，
// 输出必须经 ‥wparam 零拷贝重定向落到**调用方**的槽位。
func TestDelegateWriteParamRedirect(t *testing.T) {
	kv := newKV(t)
	defer startBackend(t, kv, "fake", "echo")()

	loadSrc(t, kv, `
rwir fake.echo(a:string) -> (b:string)

rwfunc wrap(x:string) -> (y:string) {
	fake.echo(x) -> y
}

rwfunc main() -> () {
	wrap("hello") -> r
}

main()
`)
	vtid := run(t, kv, "init")

	got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/r"))
	if got.String() != "hello" {
		t.Fatalf("嵌套帧写参重定向失败：r=%q，期望 \"hello\"", got.String())
	}
}

// TestDelegateTaskIDUniquePerIteration 覆盖循环内 taskID 碰撞：
// scope 帧每轮复用同一 PC，仅靠 (vtid, pc) 导出 taskID 会每轮撞号，
// 叠加持久化的 Notify 队列会让下一轮读走上一轮迟到的完成信号。
func TestDelegateTaskIDUniquePerIteration(t *testing.T) {
	kv := newKV(t)
	defer startBackend(t, kv, "fake", "echo")()

	loadSrc(t, kv, `
rwir fake.echo(a:string) -> (b:string)

rwfunc main() -> () {
	i = 0
	while (i < 3) {
		fake.echo("v") -> out
		i + 1 -> i
	}
}

main()
`)
	vtid := run(t, kv, "init")

	seq := kvspace.GetOne(kv, keytree.VThreadAt(vtid, keytree.RuntimeMemberSep+"delegseq"))
	if seq.String() != "3" {
		t.Fatalf("三轮循环应产生三个不同 taskID，delegseq=%q，期望 \"3\"", seq.String())
	}
}

// TestDelegateNoBackend 确认路由失败会写 vthread 错误状态。
func TestDelegateNoBackend(t *testing.T) {
	kv := newKV(t)

	loadSrc(t, kv, `
rwir nobody.op(a:string) -> (b:string)

rwfunc main() -> () {
	nobody.op("x") -> r
}

main()
`)
	vtid := vthread.AllocVtid(kv)
	kv.DelTree(keytree.VThread(vtid))
	kvspace.MkIndexRecursive(kv, keytree.VThread(vtid)+keytree.PathSegSep)
	builtin.WriteSysRwir(kv)
	pc := layout.Bootstrap(context.Background(), kv, vtid, "main", nil)
	vthread.Set(context.Background(), kv, vtid, pc, "init")

	if err := kvcpu.New(kv, "test").Execute(pc); err == nil {
		t.Fatal("无后端时 Execute 应返回错误")
	}
	msg := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error"))
	if !strings.Contains(msg.String(), "no backend supports") {
		t.Fatalf("‥error/msg 未写入路由失败原因，实得 %q", msg.String())
	}
}

// TestIsDelegatedDistinguishesKind 确认判据是声明（kind=rwir）而非命名空间前缀，
// 因此 `lib tensor { }` 这类用户库不会被误判为委托。
func TestIsDelegatedDistinguishesKind(t *testing.T) {
	kv := newKV(t)
	loadSrc(t, kv, `
rwir ext.op(a:string) -> (b:string)

lib tensor {
	rwfunc matmul(a:string) -> (b:string) { a -> b }
}

rwfunc plain(a:string) -> (b:string) { a -> b }
`)
	for _, tc := range []struct {
		opcode string
		want   bool
	}{
		{"ext.op", true},
		{"tensor.matmul", false}, // 用户库，kind=rwfunc
		{"plain", false},
		{"nosuch", false}, // 不存在
	} {
		if got := dispatch.IsDelegated(kv, tc.opcode); got != tc.want {
			t.Errorf("IsDelegated(%q) = %v，期望 %v", tc.opcode, got, tc.want)
		}
	}
}
