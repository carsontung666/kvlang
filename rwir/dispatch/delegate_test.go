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

// backend 是测试用的 echo 执行器。seen 记录它见过的全部 taskID。
type backend struct {
	seen     []string
	done     chan struct{}
	finished chan struct{}
}

// stop 关停执行器并**等它真的退出** —— 否则残留的 watcher 会和下一个测试
// 抢同一条 cmd 队列，且 t.Errorf 可能在测试结束后触发 panic。
func (b *backend) stop() {
	close(b.done)
	<-b.finished
}

// startBackend 起一个 echo 执行器：注册能力，然后循环消费命令队列。
func startBackend(t *testing.T, kv kvspace.KVSpace, name, op string) *backend {
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

	b := &backend{done: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer close(b.finished)
		queue := keytree.SysOpCmd(name, "0")
		for {
			select {
			case <-b.done:
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
			b.seen = append(b.seen, task.ID)
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
	return b
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

// newKV 返回一个干净的 kvspace。
//
// art:// 是**进程内全局单例** —— Conn 不会给出独立实例，DisConn 也不清空。
// 不显式清理的话，上一个测试残留的 /lib/main 会串进下一个测试，`go test -shuffle=on`
// 就会随机失败。必须清根，不能只建索引。
func newKV(t *testing.T) kvspace.KVSpace {
	t.Helper()
	kv := kvspace.Conn("art://")
	if kv == nil {
		t.Fatal("Conn(art://) 返回 nil")
	}
	for _, root := range []string{keytree.LibRoot, keytree.VthreadRoot, keytree.SysOpRoot,
		keytree.SysRoot + keytree.PathSegSep + keytree.SegTask} {
		kv.DelTree(root)
	}
	kvspace.MkIndexRecursive(kv, keytree.LibRoot+keytree.PathSegSep)
	kvspace.MkIndexRecursive(kv, keytree.VthreadRoot+keytree.PathSegSep)
	return kv
}

// TestDelegateWriteParamRedirect 是核心用例：委托指令出现在嵌套 rwfunc 帧内时，
// 输出必须经 ‥wparam 零拷贝重定向落到**调用方**的槽位。
func TestDelegateWriteParamRedirect(t *testing.T) {
	kv := newKV(t)
	be := startBackend(t, kv, "fake", "echo")
	defer be.stop()

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
	be := startBackend(t, kv, "fake", "echo")

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
	be.stop()

	if len(be.seen) != 3 {
		t.Fatalf("后端应收到 3 个任务，实得 %d 个：%v", len(be.seen), be.seen)
	}
	uniq := map[string]bool{}
	for _, id := range be.seen {
		uniq[id] = true
	}
	if len(uniq) != 3 {
		t.Fatalf("三轮的 taskID 必须互不相同，实得 %v", be.seen)
	}
	// 顺带确认循环体真的跑了、结果真的写回了
	if got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/out")); got.String() != "v" {
		t.Fatalf("out=%q，期望 \"v\"", got.String())
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
	if pc == "" {
		t.Fatal("Bootstrap main failed")
	}
	vthread.Set(context.Background(), kv, vtid, pc, "init")

	err := kvcpu.New(kv, "test").Execute(pc)
	if err == nil || !strings.Contains(err.Error(), "no backend supports") {
		t.Fatalf("期望路由失败错误，实得 %v", err)
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

// TestDelegateRejectsProtectedWriteSlot 覆盖对抗测试实证出的三条劫持路径：
// 委托的写槽落到引擎保留键（‥returnpc 可劫持控制流、写非 PC 值让 VM 裸 panic）、
// 落到 /lib（改写运行中的代码）、落到 /dev（device 层会把它当文件路径 open）。
// 这些必须在派发**之前**被拒 —— 一旦 outputs 出了门，写就发生在 VM 之外。
func TestDelegateRejectsProtectedWriteSlot(t *testing.T) {
	for _, tc := range []struct{ name, slot, want string }{
		{"引擎保留键", "/vthread/1/‥returnpc", "引擎保留键"},
		{"帧内保留键", "/vthread/1/[0,0]/‥ro", "引擎保留键"},
		{"运行中的代码", "/lib/main/[1,0]", "受保护域"},
		{"后端注册表", "/sys/op/evil/0", "受保护域"},
		{"设备层", "/dev/tty/x/stdout/detail", "受保护域"},
		{"别的 vthread", "/vthread/999/r", "其它 vthread"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := dispatch.CheckWriteKeyForTest("1", tc.slot); err == nil {
				t.Fatalf("写槽 %q 应被拒绝", tc.slot)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误原因应含 %q，实得 %v", tc.want, err)
			}
		})
	}
	// 合法落点必须放行：自己帧内的槽位、用户全局键
	for _, ok := range []string{"/vthread/1/[0,0]/r", "/vthread/1/x", "/n1", "/mydata/x"} {
		if err := dispatch.CheckWriteKeyForTest("1", ok); err != nil {
			t.Errorf("合法写槽 %q 被误拒: %v", ok, err)
		}
	}
}

// TestSelectNamespaceIsBackend 确认命名空间即后端名：找不到就报错，
// 不静默回退到某个碰巧也注册了同名算子的后端。
func TestSelectNamespaceIsBackend(t *testing.T) {
	kv := newKV(t)
	startBackend(t, kv, "llm", "complete").stop() // llm 只支持 complete
	startBackend(t, kv, "other", "chat").stop()   // other 支持 chat

	if _, _, err := dispatch.Select(context.Background(), kv, "llm.chat"); err == nil {
		t.Fatal("llm 不支持 chat 时应报错，而不是把任务投给 other")
	}
	b, _, err := dispatch.Select(context.Background(), kv, "other.chat")
	if err != nil || b != "other" {
		t.Fatalf("other.chat 应选中 other，实得 backend=%q err=%v", b, err)
	}
}

// TestSelectSkipsStoppedInstance 确认候选后端的实例不可用时会继续找别的候选，
// 而不是锁定第一个候选后硬失败。
func TestSelectSkipsStoppedInstance(t *testing.T) {
	kv := newKV(t)
	// aaa 注册了 echo 但实例是 stopped；zzz 注册了 echo 且 running
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysOpFunc("aaa", "echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysOp("aaa", "0"), Val: kvspace.NewChar(`{"status":"stopped","load":0}`)},
	})
	startBackend(t, kv, "zzz", "echo").stop()

	b, _, err := dispatch.Select(context.Background(), kv, "echo")
	if err != nil || b != "zzz" {
		t.Fatalf("应跳过 stopped 的 aaa 选中 zzz，实得 backend=%q err=%v", b, err)
	}
}
