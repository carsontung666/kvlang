package dispatch_test

// 委托 rwir 的端到端测试：源码 → layout → 执行 → 派发给外部执行器 → 写回 → 续跑。
//
// 执行器在这里是一个 goroutine 而非独立进程（art:// 是进程内 kvspace），
// 但它只通过 kvspace 与 VM 交互，走的是与真实外部进程完全相同的协议。
// 真正的跨进程验收在 examples/delegate/run.sh。

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
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

// ── 脚手架 ────────────────────────────────────────────────────────────────

// newKV 返回一个干净的 kvspace。
//
// art:// 是**进程内全局单例** —— Conn 不给独立实例，DisConn 也不清空。
// 不显式清理的话，上一个用例残留的 /lib/main 会串进下一个用例，
// `go test -shuffle=on` 就会随机失败。
//
// 清 /vthread 根会连 ‥seq 计数器一起清掉 → vtid 每个用例都从 1 重来 →
// taskID 跨用例重复。而 Notify 队列活在 kvspace 树外（__notify: 命名空间），
// DelTree 删不掉，于是上一个用例遗留的 done 信号会被下一个用例读走。
// 解法是清完把 ‥seq 顶到一个单调递增的基数上。
// 这不是测试专属的怪癖 —— 任何清空 /vthread 的操作都会踩到。
var vtidBase = 1000

func newKV(t *testing.T) kvspace.KVSpace {
	t.Helper()
	kv := kvspace.Conn("art://")
	if kv == nil {
		t.Fatal("Conn(art://) 返回 nil")
	}
	for _, root := range []string{keytree.LibRoot, keytree.VthreadRoot, keytree.SysOpRoot, keytree.SysTaskRoot} {
		kv.DelTree(root)
	}
	kvspace.MkIndexRecursive(kv, keytree.LibRoot+keytree.PathSegSep)
	kvspace.MkIndexRecursive(kv, keytree.VthreadRoot+keytree.PathSegSep)
	vtidBase += 100
	kv.Set([]kvspace.KVPair{{Key: keytree.VthreadSeq, Val: kvspace.NewChar(strconv.Itoa(vtidBase))}})
	return kv
}

// loadSrc 把源码编译进 kvspace，照搬 cmd/kvlang 的装载流程：顶层调用包成
// /lib/init，执行入口是 init 而非某个具体函数 —— 直接 Bootstrap 一个普通
// rwfunc 会绕过这条路径，控制流 scope 帧只会被布局、不会被执行。
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
		fpkg := df.Funcs[i].Pkg
		if fpkg == "" {
			fpkg = df.Package
		}
		layout.WriteFunc(kv, fpkg, lower.Func(&df.Funcs[i]))
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

// start 创建 vthread 并执行 entry，返回 vtid 与 Execute 的错误（不 Fatal）。
// 与 cmd/kvlang/run.go 的 executeEntry 同一流程。
func start(t *testing.T, kv kvspace.KVSpace, entry string) (string, error) {
	t.Helper()
	ctx := context.Background()
	vtid := vthread.AllocVtid(kv)
	kv.DelTree(keytree.VThread(vtid))
	kvspace.MkIndexRecursive(kv, keytree.VThread(vtid)+keytree.PathSegSep)
	builtin.WriteSysRwir(kv)
	pc := layout.Bootstrap(ctx, kv, vtid, entry, nil)
	if pc == "" {
		t.Fatalf("Bootstrap %s failed", entry)
	}
	vthread.Set(ctx, kv, vtid, pc, "init")
	return vtid, kvcpu.New(kv, "test").Execute(pc)
}

// run 执行 entry 并要求成功。
func run(t *testing.T, kv kvspace.KVSpace, entry string) string {
	t.Helper()
	vtid, err := start(t, kv, entry)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return vtid
}

// backendCfg 是执行器的行为配置。
//
// 必须在 goroutine 起来**之前**定下来：跑起来之后再改字段就是数据竞争，
// -race 会直接报（本文件的测试就踩过一次）。要模拟"跑到一半开始罢工"，
// 用 writeFirstN 这种由消费 goroutine 自己计数的方式，别从外面翻开关。
type backendCfg struct {
	status      string // 上报给 VM 的完成状态，默认 "done"
	writeFirstN int    // 只给前 N 个任务写输出槽；-1 = 全写
}

// backend 是测试用的 echo 执行器。
// seen/raw 只在 stop() 之后读（stop 会等消费 goroutine 退出），无需加锁。
type backend struct {
	seen     []dispatch.OpTask // 解析后的任务，按收到顺序
	raw      []string          // 队列里的原始 JSON 字符串，用于钉住线协议
	cfg      backendCfg
	inst     string
	done     chan struct{}
	finished chan struct{}
}

// taskIDs 返回执行器见过的全部 taskID（需在 stop 之后调用）。
func (b *backend) taskIDs() []string {
	ids := make([]string, 0, len(b.seen))
	for _, t := range b.seen {
		ids = append(ids, t.ID)
	}
	return ids
}

// stop 关停执行器并**等它真的退出** —— 否则残留的 watcher 会和下一个用例
// 抢同一条 cmd 队列，且 t.Errorf 可能在测试结束后触发 panic。
func (b *backend) stop() {
	select {
	case <-b.done: // 已经停过
	default:
		close(b.done)
	}
	<-b.finished
}

// instSeq 让每个用例的实例编号唯一。命令队列 /sys/op/<b>/<n>/cmd 是 Notify
// 队列，活在 kvspace 树外，DelTree 清不掉。用例若留下未被消费的任务（比如
// 超时用例），固定用 "0" 会让下一个用例的执行器把它捞走。
var instSeq = 0

// registerBackend 注册能力与一个 running 实例，返回实例编号。
func registerBackend(t *testing.T, kv kvspace.KVSpace, name, op string) string {
	t.Helper()
	instSeq++
	inst := strconv.Itoa(instSeq)
	if err := kv.Set([]kvspace.KVPair{
		{Key: keytree.SysOpFunc(name, op), Val: kvspace.NewChar("1")},
		{Key: keytree.SysOp(name, inst), Val: kvspace.NewChar(`{"status":"running","load":0}`)},
	}); err != nil {
		t.Fatalf("register %s.%s: %v", name, op, err)
	}
	return inst
}

// startBackend 起一个 echo 执行器：注册能力，然后循环消费命令队列。
// 协议顺序与 examples/delegate/fake_backend.py 完全一致：写 outputs → 置
// .status → Notify .done。
func startBackend(t *testing.T, kv kvspace.KVSpace, name, op string) *backend {
	return startBackendCfg(t, kv, name, op, backendCfg{status: "done", writeFirstN: -1})
}

// startBackendCfg 起一个行为可配置的执行器。配置在 goroutine 启动前固定。
func startBackendCfg(t *testing.T, kv kvspace.KVSpace, name, op string, cfg backendCfg) *backend {
	t.Helper()
	inst := registerBackend(t, kv, name, op)

	b := &backend{cfg: cfg, inst: inst, done: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer close(b.finished)
		queue := keytree.SysOpCmd(name, inst)
		handled := 0
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
				// 若报 "invalid character '\x00'"，说明 VM 侧用了 NewBytes 而非
				// NewChar —— NewBytes 给每个元素追加一个 NUL。
				t.Errorf("backend: 任务 JSON 无法解析 %q: %v", raw.String(), err)
				continue
			}
			b.raw = append(b.raw, raw.String())
			b.seen = append(b.seen, task)
			var val string
			if len(task.Inputs) > 0 {
				val = task.Inputs[0].Value
			}
			handled++
			if b.cfg.writeFirstN < 0 || handled <= b.cfg.writeFirstN {
				for _, out := range task.Outputs {
					kv.Set([]kvspace.KVPair{{Key: out.Key, Val: kvspace.NewChar(val)}})
				}
			}
			kv.Set([]kvspace.KVPair{
				{Key: keytree.SysTask(task.ID, "status"), Val: kvspace.NewChar(b.cfg.status)},
			})
			kv.Notify(task.DoneKey, kvspace.NewChar("1"))
		}
	}()
	return b
}

// ── 装载：声明必须真的落进 /lib ────────────────────────────────────────────

// TestWriteDeclsRegistersRwirKind 确认 rwir 声明被装载进 /lib 且 kind 为 rwir。
// 此前 WriteRwir 无人调用，声明解析后即丢弃。
func TestWriteDeclsRegistersRwirKind(t *testing.T) {
	kv := newKV(t)
	loadSrc(t, kv, `
rwir fake.echo(a:string) -> (b:string)

lib llm {
	rwir chat(prompt:string) -> (reply:string)
}
`)
	for _, key := range []string{"/lib/fake.echo", "/lib/llm.chat"} {
		v := kvspace.GetOne(kv, key)
		if kvspace.IsNone(v) {
			t.Fatalf("%s 未写入 /lib —— 声明被静默丢弃", key)
		}
		if v.Kind() != kvspace.KindRwir {
			t.Fatalf("%s 的 kind = %q，期望 %q", key, v.Kind(), kvspace.KindRwir)
		}
	}
}

// TestRwirDeclInLibDoesNotSwallowProgram 是回归用例：parseLibBody 的 rwir 分支
// 缺 continue 会落到 parseStmt，往 InitBody 塞一条幻影空指令 → /lib/init/[0,0]
// 是空 opcode → vthread 立刻 SetDone("ok") → 整个程序静默变成空操作。
// 断言的是「后面的语句真的执行了」，不是「解析没报错」。
func TestRwirDeclInLibDoesNotSwallowProgram(t *testing.T) {
	kv := newKV(t)
	loadSrc(t, kv, `
lib llm {
	rwir chat(prompt:string) -> (reply:string)
}

rwfunc main() -> () {
	40 + 2 -> answer
}

main()
`)
	vtid := run(t, kv, "init")
	got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/answer"))
	if got.String() != "42" {
		t.Fatalf("lib 内的 rwir 声明吞掉了后续执行：answer=%q，期望 \"42\"", got.String())
	}
}

// ── 委托主路径 ────────────────────────────────────────────────────────────

// TestDelegateWriteParamRedirect 是核心用例：委托指令出现在嵌套 rwfunc 帧内时，
// 输出必须经 ‥wparam 零拷贝重定向落到**调用方**的槽位，输入必须是变量的**值**。
//
// 旧实现两处都错：写参用 keytree.VThreadAt(vtid, name) 拼到 vthread 根帧；
// 读参被 isLiteral() 判成字面量，塞进任务的是变量名 "x" 而不是 "hello"。
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

// TestDelegateResolvesLocalVariable 确认读参取的是局部变量的值而非变量名。
func TestDelegateResolvesLocalVariable(t *testing.T) {
	kv := newKV(t)
	be := startBackend(t, kv, "fake", "echo")
	defer be.stop()

	loadSrc(t, kv, `
rwir fake.echo(a:string) -> (b:string)

rwfunc main() -> () {
	"payload" -> v
	fake.echo(v) -> out
}

main()
`)
	vtid := run(t, kv, "init")
	if got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/out")); got.String() != "payload" {
		t.Fatalf("局部变量未被解析为值：out=%q，期望 \"payload\"", got.String())
	}
}

// TestDelegateTaskIDUniquePerIteration 覆盖循环内 taskID 碰撞：
// scope 帧每轮复用同一 PC，仅靠 (vtid, pc) 导出 taskID 会每轮撞号，
// 叠加持久化的 Notify 队列会让下一轮读走上一轮迟到的完成信号。
//
// 断言的是**执行器实际收到的 ID**（可观测行为），不是内部计数器 ——
// 让计数器恒返 1 的实现照样能通过「‥delegseq == 3」这种断言。
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

	seen := be.taskIDs()
	if len(seen) != 3 {
		t.Fatalf("执行器应收到 3 个任务，实得 %d 个：%v", len(seen), seen)
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	if len(uniq) != 3 {
		t.Fatalf("三轮的 taskID 必须互不相同，实得 %v", seen)
	}
	// 顺带确认循环体真的跑了、结果真的写回了
	if got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/out")); got.String() != "v" {
		t.Fatalf("out=%q，期望 \"v\"", got.String())
	}
}

// TestOpTaskWireFormat 钉住投递给执行器的 **JSON 线协议**。
//
// 为什么必须按原始字符串断言：测试里的执行器用的是 VM 自己的 dispatch.OpTask
// 反序列化，于是改 json tag（done_key→doneKey）、把 opcode/vtid/pc 置空，
// 在 Go 侧全都察觉不到，而 examples/delegate/fake_backend.py 会当场崩掉。
// 跨进程契约只能靠 map[string]any 上的断言守住。
func TestOpTaskWireFormat(t *testing.T) {
	kv := newKV(t)
	be := startBackend(t, kv, "fake", "echo")
	loadSrc(t, kv, `
rwir fake.echo(a:string) -> (b:string)

rwfunc main() -> () {
	fake.echo("payload") -> out
}

main()
`)
	vtid := run(t, kv, "init")
	be.stop()

	if len(be.raw) != 1 {
		t.Fatalf("执行器应收到 1 个任务，实得 %d", len(be.raw))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(be.raw[0]), &m); err != nil {
		t.Fatalf("任务不是合法 JSON 对象: %v；原始载荷 %q", err, be.raw[0])
	}

	// 字段名逐个钉死 —— 外部执行器按这些名字取值
	want := []string{"id", "vtid", "pc", "opcode", "inputs", "outputs", "done_key"}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("线协议缺字段 %q；实得字段集 %v", k, keysOf(m))
		}
	}
	for k := range m {
		if !contains(want, k) {
			t.Errorf("线协议多出未声明字段 %q —— 加字段要同步改 fake_backend.py", k)
		}
	}

	// 值也要对：置空 opcode/vtid/pc 不该悄悄通过
	if m["opcode"] != "fake.echo" {
		t.Errorf("opcode = %v，期望 \"fake.echo\"", m["opcode"])
	}
	if m["vtid"] != vtid {
		t.Errorf("vtid = %v，期望 %q", m["vtid"], vtid)
	}
	if pc, _ := m["pc"].(string); pc == "" {
		t.Error("pc 为空 —— 执行器靠它定位调用点")
	}
	// done_key 必须由 taskID 导出，不能是 vthread 级别的共享键：
	// 共享的话每轮循环又会读到上一轮迟到的信号，正是 taskID 带序号要防的事
	id, _ := m["id"].(string)
	if got, want := m["done_key"], keytree.SysTask(id, "done"); got != want {
		t.Errorf("done_key = %v，期望 %q（必须由 taskID 导出）", got, want)
	}
	// inputs 传值、outputs 传绝对键
	ins, _ := m["inputs"].([]any)
	if len(ins) != 1 {
		t.Fatalf("inputs 应有 1 项，实得 %v", m["inputs"])
	}
	if v := ins[0].(map[string]any)["value"]; v != "payload" {
		t.Errorf("inputs[0].value = %v，期望 \"payload\"", v)
	}
	outs, _ := m["outputs"].([]any)
	if len(outs) != 1 {
		t.Fatalf("outputs 应有 1 项，实得 %v", m["outputs"])
	}
	if k, _ := outs[0].(map[string]any)["key"].(string); !strings.HasPrefix(k, keytree.VThread(vtid)) {
		t.Errorf("outputs[0].key = %q，应是本 vthread 内的绝对键", k)
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// TestTaskIDCarriesVtid 确认 taskID 由 vtid + 每-vthread 自增序号两部分组成。
//
// 只断言"同一个 vthread 内每轮 ID 不同"是不够的：去掉 vtid 前缀、或把每-vthread
// 计数器换成全局计数器，单 vthread 的用例都察觉不到，而两个 vthread 并发委托
// 就会撞号。这里同时钉住两个组成部分。
func TestTaskIDCarriesVtid(t *testing.T) {
	kv := newKV(t)
	be := startBackend(t, kv, "fake", "echo")
	loadSrc(t, kv, delegateSrc)

	vtidA := run(t, kv, "init")
	vtidB := run(t, kv, "init") // 第二个 vthread，跑同一份代码
	be.stop()

	ids := be.taskIDs()
	if len(ids) != 2 {
		t.Fatalf("两个 vthread 各委托一次，应收到 2 个任务，实得 %v", ids)
	}
	if vtidA == vtidB {
		t.Fatalf("两次 run 应得到不同 vtid，实得都是 %q", vtidA)
	}
	for i, vtid := range []string{vtidA, vtidB} {
		if !strings.HasPrefix(ids[i], vtid+"-") {
			t.Errorf("taskID %q 未带 vtid 前缀 %q —— 跨 vthread 会撞号", ids[i], vtid)
		}
	}
	if ids[0] == ids[1] {
		t.Fatalf("两个 vthread 的 taskID 撞号：%v", ids)
	}
	// 序号必须是每 vthread 一个：全局计数器的话 B 的序号会接着 A 往下走
	for _, vtid := range []string{vtidA, vtidB} {
		if got := kvspace.GetOne(kv, keytree.VThreadDelegSeq(vtid)).String(); got != "1" {
			t.Errorf("vthread %s 的委托序号 = %q，期望 \"1\"（每 vthread 独立计数）", vtid, got)
		}
	}
}

// TestDelegateTaskLifecycle 钉住包头声明的任务对象生命周期：
// 派发前置 pending，成功后回收，失败后留痕。
func TestDelegateTaskLifecycle(t *testing.T) {
	t.Run("成功后回收 status 键", func(t *testing.T) {
		kv := newKV(t)
		be := startBackend(t, kv, "fake", "echo")
		loadSrc(t, kv, delegateSrc)
		run(t, kv, "init")
		be.stop()

		id := be.taskIDs()[0]
		if v := kvspace.GetOne(kv, keytree.SysTask(id, "status")); !kvspace.IsNone(v) {
			t.Fatalf("成功后 /sys/task/%s.status 应被回收，实得 %q", id, v.String())
		}
	})

	t.Run("超时后留痕且停在 pending", func(t *testing.T) {
		defer dispatch.SetTimeoutForTest(150 * time.Millisecond)()
		kv := newKV(t)
		registerBackend(t, kv, "fake", "echo") // 注册但不消费
		loadSrc(t, kv, delegateSrc)

		vtid, err := start(t, kv, "init")
		if err == nil {
			t.Fatal("执行器不响应时应报错")
		}
		// taskID 是可预测的：vtid + 第 1 号委托
		id := vtid + "-1"
		if v := kvspace.GetOne(kv, keytree.SysTask(id, "status")); v.String() != "pending" {
			t.Fatalf("超时后 /sys/task/%s.status 应留着 \"pending\" 供排查，实得 %q", id, v.String())
		}
	})
}

// TestDelegateSetsWaitStatus 确认派发期间 vthread 处于 wait 态。
// 外部观察者（kvlang ps / 调试器）就靠这个状态看出 VM 阻塞在委托上。
func TestDelegateSetsWaitStatus(t *testing.T) {
	defer dispatch.SetTimeoutForTest(2 * time.Second)()
	kv := newKV(t)
	registerBackend(t, kv, "fake", "echo") // 注册但不消费 → VM 会一直等
	loadSrc(t, kv, delegateSrc)

	seen := make(chan string, 1)
	go func() {
		deadline := time.Now().Add(1500 * time.Millisecond)
		for time.Now().Before(deadline) {
			// vtid 可预测：newKV 把 ‥seq 顶到 vtidBase，AllocVtid 返回 vtidBase+1
			v := kvspace.GetOne(kv, keytree.VThreadStatus(strconv.Itoa(vtidBase+1)))
			if v.String() == "wait" {
				seen <- "wait"
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		seen <- ""
	}()

	start(t, kv, "init") // 会超时报错，这里不关心
	if got := <-seen; got != "wait" {
		t.Fatal("派发期间 vthread 状态应为 \"wait\"，整个窗口内一次都没观察到")
	}
}

// ── 失败路径 ──────────────────────────────────────────────────────────────

// TestDelegateNoBackend 确认路由失败会写 vthread 错误状态。
// 只 return error 不写 ‥error/msg 的话，cmd/kvlang/run.go 的 reportRunError
// 读不到东西，于是静默失败且 exit 0。
func TestDelegateNoBackend(t *testing.T) {
	kv := newKV(t)
	loadSrc(t, kv, `
rwir nobody.op(a:string) -> (b:string)

rwfunc main() -> () {
	nobody.op("x") -> r
}

main()
`)
	vtid, err := start(t, kv, "init")
	if err == nil || !strings.Contains(err.Error(), "no backend supports") {
		t.Fatalf("期望路由失败错误，实得 %v", err)
	}
	msg := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error"))
	if !strings.Contains(msg.String(), "no backend supports") {
		t.Fatalf("‥error/msg 未写入路由失败原因（reportRunError 会 exit 0），实得 %q", msg.String())
	}
}

const delegateSrc = `
rwir fake.echo(a:string) -> (b:string)

rwfunc main() -> () {
	fake.echo("v") -> out
}

main()
`

// TestDelegateTimeout 覆盖执行器完全不响应：必须干净报错并写 ‥error/msg，
// 而不是挂死或误判成功。
func TestDelegateTimeout(t *testing.T) {
	defer dispatch.SetTimeoutForTest(150 * time.Millisecond)()
	kv := newKV(t)
	registerBackend(t, kv, "fake", "echo") // 注册但不起消费者：任务推进队列后无人处理
	loadSrc(t, kv, delegateSrc)

	vtid, err := start(t, kv, "init")
	if err == nil || !strings.Contains(err.Error(), "timeout or failed") {
		t.Fatalf("执行器不响应时应超时报错，实得 %v", err)
	}
	if msg := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error")); !strings.Contains(msg.String(), "timeout or failed") {
		t.Fatalf("‥error/msg 未写入超时原因，实得 %q", msg.String())
	}
}

// TestDelegateBackendReportsFailed 覆盖执行器主动报失败：VM 必须终止而非继续。
// 这条同时锁住「Watch 返回后复查 .status」—— 删掉复查这条就会绿变红。
func TestDelegateBackendReportsFailed(t *testing.T) {
	defer dispatch.SetTimeoutForTest(2 * time.Second)()
	kv := newKV(t)
	be := startBackendCfg(t, kv, "fake", "echo", backendCfg{status: "failed", writeFirstN: -1})
	loadSrc(t, kv, delegateSrc)

	_, err := start(t, kv, "init")
	be.stop()
	if err == nil || !strings.Contains(err.Error(), `status="failed"`) {
		t.Fatalf("执行器报 failed 时应终止并带上状态，实得 %v", err)
	}
}

// TestDelegateReportsDoneWithoutWriting 覆盖不守协议的执行器：报了 done
// 却没写输出槽。VM 必须判失败，而不是拿着空值继续跑然后 exit 0 —— 静默
// 错误结果比崩掉难查得多。
func TestDelegateReportsDoneWithoutWriting(t *testing.T) {
	defer dispatch.SetTimeoutForTest(2 * time.Second)()
	kv := newKV(t)
	be := startBackendCfg(t, kv, "fake", "echo", backendCfg{status: "done", writeFirstN: 0})
	loadSrc(t, kv, delegateSrc)

	vtid, err := start(t, kv, "init")
	be.stop()

	if err == nil || !strings.Contains(err.Error(), "没写输出槽") {
		t.Fatalf("执行器报 done 却没写输出时应判失败，实得 %v", err)
	}
	if msg := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error")); !strings.Contains(msg.String(), "没写输出槽") {
		t.Fatalf("‥error/msg 未写入原因（reportRunError 会 exit 0），实得 %q", msg.String())
	}
	if v := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/out")); !kvspace.IsNone(v) {
		t.Fatalf("输出槽本不该有值，实得 %q", v.String())
	}
}

// TestDelegateStaleOutputNotMistakenForSuccess 是上一条在循环里的版本：
// 第 1 轮写了值，第 2 轮执行器罢工。派发前若不清写槽，第 2 轮会读到第 1 轮
// 的残值并误判成功 —— 循环里每轮复用同一个槽，这是最容易漏的一种。
func TestDelegateStaleOutputNotMistakenForSuccess(t *testing.T) {
	defer dispatch.SetTimeoutForTest(2 * time.Second)()
	kv := newKV(t)
	// 只给第 1 个任务写输出，第 2 轮罢工。计数在消费 goroutine 内部完成，
	// 外面不碰任何共享字段。
	be := startBackendCfg(t, kv, "fake", "echo", backendCfg{status: "done", writeFirstN: 1})
	loadSrc(t, kv, `
rwir fake.echo(a:string) -> (b:string)

rwfunc main() -> () {
	i = 0
	while (i < 2) {
		fake.echo("v") -> out
		i + 1 -> i
	}
}

main()
`)
	_, err := start(t, kv, "init")
	be.stop()
	if err == nil || !strings.Contains(err.Error(), "没写输出槽") {
		t.Fatalf("第 2 轮执行器罢工时应判失败（不能拿第 1 轮的残值），实得 %v", err)
	}
}

// TestDelegateArityMismatch 确认调用点与声明的读写参个数不符时拒绝派发。
// 委托调用不经 HandleCall，没人按声明绑参 —— 少了这道检查就会把畸形任务
// 静默发给外部系统。断言"执行器一个任务都没收到"，而不只是"报了错"。
func TestDelegateArityMismatch(t *testing.T) {
	for _, tc := range []struct{ name, call, want string }{
		{"读参过多", `fake.echo("A", "B", "C") -> z`, "读参个数不符"},
		{"读参过少", `fake.echo() -> z`, "读参个数不符"},
		{"写参过少", `fake.echo("A")`, "写参个数不符"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := newKV(t)
			be := startBackend(t, kv, "fake", "echo")
			loadSrc(t, kv, "rwir fake.echo(a:string) -> (b:string)\n\n"+
				"rwfunc main() -> () {\n\t"+tc.call+"\n}\n\nmain()\n")

			vtid, err := start(t, kv, "init")
			be.stop()

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报 %q，实得 %v", tc.want, err)
			}
			if msg := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error")); !strings.Contains(msg.String(), tc.want) {
				t.Fatalf("‥error/msg 未写入原因，实得 %q", msg.String())
			}
			if ids := be.taskIDs(); len(ids) != 0 {
				t.Fatalf("校验必须在派发**之前**发生，执行器不该收到任务，实得 %v", ids)
			}
		})
	}
}

// TestDelegateArityMatchStillWorks 反向确认上一条没有误伤正常调用。
func TestDelegateArityMatchStillWorks(t *testing.T) {
	kv := newKV(t)
	be := startBackend(t, kv, "two", "join")
	defer be.stop()
	loadSrc(t, kv, `
rwir two.join(a:string, b:string) -> (c:string)

rwfunc main() -> () {
	two.join("x", "y") -> r
}

main()
`)
	vtid := run(t, kv, "init")
	if got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/r")); got.String() != "x" {
		t.Fatalf("两读参一写参的正常调用应通过，r=%q", got.String())
	}
}

// ── 判据与路由 ────────────────────────────────────────────────────────────

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

// TestTensorLibIsNotDelegated 端到端锁住上一条：删掉了硬编码的 tensor. 前缀
// 分支之后，用户自己的 lib tensor 必须当普通函数执行并算出结果。
func TestTensorLibIsNotDelegated(t *testing.T) {
	kv := newKV(t)
	loadSrc(t, kv, `
lib tensor {
	rwfunc matmul(a:int64, b:int64) -> (c:int64) {
		a * b -> c
	}
}

rwfunc main() -> () {
	tensor.matmul(6, 7) -> r
}

main()
`)
	vtid := run(t, kv, "init")
	if got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/r")); got.String() != "42" {
		t.Fatalf("lib tensor 应作为普通用户函数执行：r=%q，期望 \"42\"", got.String())
	}
}

// TestSelectFindsRegisteredBackend 是 Select 最基本的一条：有一个已注册且
// running 的后端时必须选中它。
//
// 此前这条路从未成功执行过 —— kv.List 对目录子项返回带尾斜杠的名字（"fake/"），
// 不剥离就拼出 /sys/op/fake//func/echo，kvspace 对非规范路径直接 panic。
func TestSelectFindsRegisteredBackend(t *testing.T) {
	kv := newKV(t)
	inst := registerBackend(t, kv, "fake", "echo")

	b, n, err := dispatch.Select(context.Background(), kv, "fake.echo")
	if err != nil || b != "fake" || n != inst {
		t.Fatalf("带命名空间：实得 (%q,%q,%v)，期望 (\"fake\",%q,nil)", b, n, err, inst)
	}
	b, n, err = dispatch.Select(context.Background(), kv, "echo")
	if err != nil || b != "fake" || n != inst {
		t.Fatalf("裸算子（扫全部后端）：实得 (%q,%q,%v)，期望 (\"fake\",%q,nil)", b, n, err, inst)
	}
}

// TestSelectMultiDotOpcodeSplitsAtLastDot 钉住多级点号算子的切分点。
// "a.b.c" 必须拆成后端 "a.b" + 算子 "c"，不是 "a" + "b.c" —— 执行器按哪个
// 规则注册决定了它到底会不会被选中，切分点变了就永远匹配不上。
func TestSelectMultiDotOpcodeSplitsAtLastDot(t *testing.T) {
	kv := newKV(t)
	inst := registerBackend(t, kv, "a.b", "c")    // 按"最后一个点"注册
	registerBackend(t, kv, "a", "b.c")            // 按"第一个点"注册的干扰项

	b, n, err := dispatch.Select(context.Background(), kv, "a.b.c")
	if err != nil || b != "a.b" || n != inst {
		t.Fatalf("a.b.c 应选中后端 \"a.b\"（实例 %s），实得 (%q,%q,%v)", inst, b, n, err)
	}
}

// TestSelectNamespaceIsBackend 确认命名空间即后端名：找不到就报错，
// 不静默回退到某个碰巧也注册了同名算子的后端。
func TestSelectNamespaceIsBackend(t *testing.T) {
	kv := newKV(t)
	registerBackend(t, kv, "llm", "complete") // llm 只支持 complete
	registerBackend(t, kv, "other", "chat")   // other 支持 chat

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
	inst := registerBackend(t, kv, "zzz", "echo")

	b, n, err := dispatch.Select(context.Background(), kv, "echo")
	if err != nil || b != "zzz" || n != inst {
		t.Fatalf("应跳过 stopped 的 aaa 选中 zzz，实得 (%q,%q,%v)", b, n, err)
	}
}

// TestSelectNoRunningInstance 覆盖「后端支持该算子、但没有一个 running 实例」。
// 少了 n == "" 那道检查，Select 会返回 ("","",nil)，Delegate 随后把任务推到
// 一个畸形队列路径上 —— 报错分支必须真的产出过一次才算数。
func TestSelectNoRunningInstance(t *testing.T) {
	kv := newKV(t)
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysOpFunc("dead", "echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysOp("dead", "0"), Val: kvspace.NewChar(`{"status":"stopped","load":0}`)},
	})
	b, n, err := dispatch.Select(context.Background(), kv, "dead.echo")
	if err == nil {
		t.Fatalf("没有 running 实例时必须报错，实得 (%q,%q,nil)", b, n)
	}
	if !strings.Contains(err.Error(), "no running instance") {
		t.Fatalf("错误信息应指明没有可用实例，实得 %v", err)
	}
}

// TestSelectPicksLowestLoad 确认真的在按负载挑实例。
// 此前所有用例的实例 load 都是 0，比较逻辑（含 >= 与 > 之别、以及到底有没有
// 记录当前最优）完全不可观测。
func TestSelectPicksLowestLoad(t *testing.T) {
	kv := newKV(t)
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysOpFunc("multi", "echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysOp("multi", "7"), Val: kvspace.NewChar(`{"status":"running","load":0.9}`)},
		{Key: keytree.SysOp("multi", "8"), Val: kvspace.NewChar(`{"status":"running","load":0.1}`)},
		{Key: keytree.SysOp("multi", "9"), Val: kvspace.NewChar(`{"status":"running","load":0.5}`)},
	})
	_, n, err := dispatch.Select(context.Background(), kv, "multi.echo")
	if err != nil || n != "8" {
		t.Fatalf("应选中 load 最低的实例 8，实得 (%q,%v)", n, err)
	}
}

// TestSelectIgnoresMalformedInstance 确认一条损坏的实例记录不会让路由 panic
// 或选错 —— 注册表是外部进程写的，一定会出现半截数据。
//
// 关键是最后那条 `{"status":"running","load":"x"}`：json.Unmarshal 会**报错但
// 仍把 Status 填成 "running"**（load 字段类型不符才失败）。用截断 JSON 测是测
// 不出来的 —— 那种情况 info 全零，后面的 status != "running" 顺手就挡掉了，
// 于是"检查 Unmarshal 错误"这行删掉也照样绿。这条记录 Load 解析成 0，是最低
// 负载，忽略错误的话它会**胜过**正常实例。
func TestSelectIgnoresMalformedInstance(t *testing.T) {
	kv := newKV(t)
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysOpFunc("mixed", "echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysOp("mixed", "80"), Val: kvspace.NewChar(`{"status":`)},                    // 截断
		{Key: keytree.SysOp("mixed", "81"), Val: kvspace.NewChar(`not json`)},                      // 非 JSON
		{Key: keytree.SysOp("mixed", "82"), Val: kvspace.NewChar(`{"status":"running","load":"x"}`)}, // 部分可解码
		{Key: keytree.SysOp("mixed", "83"), Val: kvspace.NewChar(`{"status":"running","load":0.5}`)}, // 唯一合法的
	})
	_, n, err := dispatch.Select(context.Background(), kv, "mixed.echo")
	if err != nil || n != "83" {
		t.Fatalf("应跳过全部损坏记录选中 83，实得 (%q,%v)", n, err)
	}
}

// TestSelectEqualLoadIsStable 钉住等负载时的取舍：先注册的胜出。
//
// 常见情形恰恰是"一堆实例都空闲（load 全 0）"，此时 `>=` 与 `>` 的差别就是
// 全部结果 —— 前者先到先得，后者后来居上。两种都说得通，但必须钉死一个，
// 否则哪天有人"顺手"改了比较符，路由目标会整体漂移而没有任何测试变红。
// kv.List 返回插入顺序（实测三次调用结果一致），所以"先注册"是可控的。
func TestSelectEqualLoadIsStable(t *testing.T) {
	kv := newKV(t)
	kv.Set([]kvspace.KVPair{{Key: keytree.SysOpFunc("tie", "echo"), Val: kvspace.NewChar("1")}})
	// 分两次写入以保证顺序：先 10，后 11，负载相同
	kv.Set([]kvspace.KVPair{{Key: keytree.SysOp("tie", "10"), Val: kvspace.NewChar(`{"status":"running","load":0.2}`)}})
	kv.Set([]kvspace.KVPair{{Key: keytree.SysOp("tie", "11"), Val: kvspace.NewChar(`{"status":"running","load":0.2}`)}})

	for i := 0; i < 3; i++ { // 顺带确认结果稳定，不随调用次数漂移
		_, n, err := dispatch.Select(context.Background(), kv, "tie.echo")
		if err != nil || n != "10" {
			t.Fatalf("第 %d 次：等负载应选先注册的 10，实得 (%q,%v)", i+1, n, err)
		}
	}
}

// TestSelectSkipsFuncSubtree 确认 /sys/op/<b>/func/ 不会被当成实例编号。
// List 返回的是 "func/" 而非 "func"，按 "func" 比较的话永远不匹配，
// 于是 GetOne(/sys/op/fake/func) 返回 None 才侥幸跳过 —— 换成有值就会选错。
func TestSelectSkipsFuncSubtree(t *testing.T) {
	kv := newKV(t)
	inst := registerBackend(t, kv, "fake", "echo")
	// 给 func 目录本身放一个看起来像实例记录的值
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRoot + "/op/fake/func", Val: kvspace.NewChar(`{"status":"running","load":0}`)},
	})
	b, n, err := dispatch.Select(context.Background(), kv, "echo")
	if err != nil || b != "fake" || n != inst {
		t.Fatalf("func 子树被当成实例：实得 (%q,%q,%v)，期望 (\"fake\",%q,nil)", b, n, err, inst)
	}
}
