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
	for _, root := range []string{
		keytree.LibRoot, keytree.VthreadRoot,
		keytree.SysOpRoot, keytree.SysTaskRoot, keytree.SysRwirBackendRoot,
	} {
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
	name     string // 唯一后端名（registerBackend 生成）
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

// backendSeq 让每个用例的后端名唯一。命令队列 /sys/rwir-backend/<b>/cmd 是
// Notify 队列，活在 kvspace 树外，DelTree 清不掉。用例若留下未被消费的任务
// （比如超时用例），复用同一个后端名会让下一个用例的执行器把它捞走。
//
// 一级路由之后唯一化的是**后端名**而不是实例号 —— 后端名即实例名。
var backendSeq = 0

// registerBackend 注册一个 ready 后端，返回它的唯一后端名。
// opcode 是**完整算子名**（fake.echo），注册表不拆命名空间。
func registerBackend(t *testing.T, kv kvspace.KVSpace, name, opcode string) string {
	t.Helper()
	backendSeq++
	b := name + "-" + strconv.Itoa(backendSeq)
	if err := kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp(b, opcode), Val: kvspace.NewChar("1")},
		{Key: keytree.SysRwirBackendStatus(b), Val: kvspace.NewChar("ready")},
	}); err != nil {
		t.Fatalf("register backend=%s op=%s: %v", b, opcode, err)
	}
	return b
}

// startBackend 起一个 echo 执行器：注册能力，然后循环消费命令队列。
// 协议顺序与 examples/delegate/fake_backend.py 完全一致：写 outputs → 置
// .status → Notify 完成信号。
func startBackend(t *testing.T, kv kvspace.KVSpace, name, opcode string) *backend {
	return startBackendCfg(t, kv, name, opcode, backendCfg{status: "done", writeFirstN: -1})
}

// startBackendCfg 起一个行为可配置的执行器。配置在 goroutine 启动前固定。
func startBackendCfg(t *testing.T, kv kvspace.KVSpace, name, opcode string, cfg backendCfg) *backend {
	t.Helper()
	be := registerBackend(t, kv, name, opcode)

	b := &backend{cfg: cfg, name: be, done: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer close(b.finished)
		queue := keytree.SysRwirBackendCmd(be)
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
	be := startBackend(t, kv, "fake", "fake.echo")
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
	be := startBackend(t, kv, "fake", "fake.echo")
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
	be := startBackend(t, kv, "fake", "fake.echo")

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
	be := startBackend(t, kv, "fake", "fake.echo")
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
	if got, want := m["done_key"], keytree.DoneRwir(id); got != want {
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
	be := startBackend(t, kv, "fake", "fake.echo")
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
	// 格式是 rwir:<backend>:<vtid>:<seq> —— vtid 在第 3 段，不是前缀。
	for i, vtid := range []string{vtidA, vtidB} {
		parts := strings.Split(ids[i], ":")
		if len(parts) != 4 || parts[0] != "rwir" || parts[2] != vtid {
			t.Errorf("taskID %q 不符 rwir:<backend>:<vtid>:<seq> 且 vtid=%q —— 跨 vthread 会撞号", ids[i], vtid)
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
		be := startBackend(t, kv, "fake", "fake.echo")
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
		be := registerBackend(t, kv, "fake", "fake.echo") // 注册但不消费
		loadSrc(t, kv, delegateSrc)

		vtid, err := start(t, kv, "init")
		if err == nil {
			t.Fatal("执行器不响应时应报错")
		}
		// taskID 是可预测的：rwir:<backend>:<vtid>:<第 1 号委托>
		id := "rwir:" + be + ":" + vtid + ":1"
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
	registerBackend(t, kv, "fake", "fake.echo") // 注册但不消费 → VM 会一直等
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
	if err == nil {
		t.Fatal("声明了 rwir 但没有后端时应报错")
	}
	// 断言用户真正看到的那条：cmd/kvlang 的 reportRunError 打印并据以决定退出码的
	// 是 ‥error/msg，Go error 只是内部终止信号。
	//
	// 判据是注册表，所以「声明了 rwir 但没后端」根本不会进委托分支 —— 它掉到
	// default 走 rewrite as call，由 HandleCall 认出 kind=rwir 并给出诊断。
	// 不认的话报的是 ExtIndex 的 "overlay failed"，那是实现细节，读的人一头雾水。
	msg := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error")).String()
	if !strings.Contains(msg, "没有后端支持") {
		t.Fatalf("‥error/msg 应指出真因是没有后端，实得 %q", msg)
	}
	if strings.Contains(msg, "overlay") {
		t.Fatalf("不该把 ExtIndex 的实现细节暴露给用户：%q", msg)
	}
}

// TestDeclDiagnosticDoesNotMaskRealError 确认「没有后端支持」这条诊断不会盖掉
// HandleCall 自己写的、更精确的失败原因。
//
// HandleCall 有多条返回 "" 的出口，checkDupParams 那条会写明重复参数名。
// 无条件覆盖的话，签名参数写重了的用户会被指去起执行器 —— 正是这条诊断本想
// 避免的误导，方向还反了。
func TestDeclDiagnosticDoesNotMaskRealError(t *testing.T) {
	kv := newKV(t)
	loadSrc(t, kv, `
rwir dup.op(a:string, a:string) -> (b:string)

rwfunc main() -> () {
	dup.op("x", "y") -> r
}

main()
`)
	vtid, err := start(t, kv, "init")
	if err == nil {
		t.Fatal("签名参数名重复应报错")
	}
	msg := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error")).String()
	if !strings.Contains(msg, "duplicate") {
		t.Fatalf("‥error/msg 应保留 HandleCall 写的重复参数诊断，实得 %q", msg)
	}
	if strings.Contains(msg, "起一个执行器") {
		t.Fatalf("真因是签名写重了参数，不该指人去起执行器：%q", msg)
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
	registerBackend(t, kv, "fake", "fake.echo") // 注册但不起消费者：任务推进队列后无人处理
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
	be := startBackendCfg(t, kv, "fake", "fake.echo", backendCfg{status: "failed", writeFirstN: -1})
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
	be := startBackendCfg(t, kv, "fake", "fake.echo", backendCfg{status: "done", writeFirstN: 0})
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
	be := startBackendCfg(t, kv, "fake", "fake.echo", backendCfg{status: "done", writeFirstN: 1})
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
			be := startBackend(t, kv, "fake", "fake.echo")
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
	be := startBackend(t, kv, "two", "two.join")
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

// TestDelegateOutputSlotIsWriteChecked 确认委托的输出槽同样受写落点校验。
//
// 这条最要紧：outputs 一旦随任务出了门，写就发生在 VM 之外，再也拦不住。
// 所以断言的是「执行器一个任务都没收到」——只断言"报了错"的话，把校验挪到
// 派发之后仍然会绿，而那时任务已经带着 /lib 的写目标出门了。
func TestDelegateOutputSlotIsWriteChecked(t *testing.T) {
	for _, tc := range []struct{ name, slot, want string }{
		{"代码区", "/lib/pwned", "受保护域"},
		{"后端注册表", "/sys/op/evil/0", "受保护域"},
		{"设备层", "/dev/tty/x/stdout/detail", "受保护域"},
		{"别的 vthread", "/vthread/999/stolen", "不在本 vthread 的子树内"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := newKV(t)
			be := startBackend(t, kv, "fake", "fake.echo")
			loadSrc(t, kv, "rwir fake.echo(a:string) -> (b:string)\n\n"+
				"rwfunc main() -> () {\n\tfake.echo(\"X\") -> "+tc.slot+"\n}\n\nmain()\n")

			vtid, err := start(t, kv, "init")
			be.stop()

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("写 %s 应被拒，实得 err=%v", tc.slot, err)
			}
			// 错误类名必须原样保留 PermissionError，不能被裹成
			// "RuntimeError: delegate: …"。仓库按诊断的首个 token 归类
			// （README 的 Error Cases、tutorial/error_cases/<类名>/ 目录都靠它），
			// 裹一层会让同一个违规在原生写与委托写两条路上被归成两类。
			// 只断言"含 受保护域"是不够的 —— 裹层之后那个子串仍然在。
			if !strings.HasPrefix(err.Error(), "PermissionError") {
				t.Fatalf("委托侧应原样保留 PermissionError 类名，实得 %v", err)
			}
			if v := kvspace.GetOne(kv, tc.slot); !kvspace.IsNone(v) {
				t.Fatalf("%s 不该被写入，实得 %q", tc.slot, v.String())
			}
			if ids := be.taskIDs(); len(ids) != 0 {
				t.Fatalf("校验必须在派发**之前**发生，执行器不该收到任务，实得 %v", ids)
			}
			if msg := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error")); !strings.Contains(msg.String(), "PermissionError") {
				t.Fatalf("‥error/msg 未写入拒绝原因，实得 %q", msg.String())
			}
		})
	}
}

// ── 判据与路由 ────────────────────────────────────────────────────────────

// TestIsDelegatedOpReadsRegistry 钉住判据本身：**看后端注册表，不看 /lib**。
//
// 这是本轮改动的核心语义。前一版判据是 /lib 签名键的 kind=rwir，即"源码里
// 声明了、就委托"；现在是"有后端声称能做、就委托"。两条 case 把差别钉死：
//   - declared_only：源码里有 rwir 声明，但没有后端注册 → **不**委托
//   - registered_only：源码里一个字没有，但后端注册了 → 委托
//
// 少了这两条，把 IsDelegatedOp 改回查 /lib 也能全绿。
func TestIsDelegatedOpReadsRegistry(t *testing.T) {
	kv := newKV(t)
	loadSrc(t, kv, `
rwir declared.only(a:string) -> (b:string)

lib tensor {
	rwfunc matmul(a:string) -> (b:string) { a -> b }
}

rwfunc plain(a:string) -> (b:string) { a -> b }
`)
	registerBackend(t, kv, "gpu", "registered.only")
	registerBackend(t, kv, "gpu", "tensor.matmul") // 后端盖过 /lib 里的同名 rwfunc

	for _, tc := range []struct {
		opcode string
		want   bool
		why    string
	}{
		{"registered.only", true, "后端注册了，源码没声明 —— 判据在注册表"},
		{"declared.only", false, "源码声明了但没后端 —— 声明不构成委托"},
		{"tensor.matmul", true, "后端注册会盖过 /lib 里的同名 rwfunc"},
		{"plain", false, "普通用户函数"},
		{"nosuch", false, "两边都没有"},
	} {
		if got := dispatch.IsDelegatedOp(kv, tc.opcode); got != tc.want {
			t.Errorf("IsDelegatedOp(%q) = %v，期望 %v（%s）", tc.opcode, got, tc.want, tc.why)
		}
	}
}

// TestDelegateWithoutRwirDecl 端到端锁住"没有 /lib 声明也能委托"。
//
// 判据从声明搬到注册表之后，这条才成立；它同时证明 Delegate 里的参数个数校验
// 在没有签名时是放行而不是硬失败。
func TestDelegateWithoutRwirDecl(t *testing.T) {
	kv := newKV(t)
	be := startBackend(t, kv, "gpu", "gpu.blur")
	defer be.stop()

	// 注意：源码里**没有** `rwir gpu.blur(...)` 声明
	loadSrc(t, kv, `
rwfunc main() -> () {
	gpu.blur("img") -> out
}

main()
`)
	vtid := run(t, kv, "init")
	if got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/out")); got.String() != "img" {
		t.Fatalf("无声明的 opcode 应照样委托：out=%q，期望 \"img\"", got.String())
	}
}

// TestTensorLibIsNotDelegated：没有后端注册 tensor.matmul 时，用户自己的
// lib tensor 必须当普通函数执行并算出结果。
//
// 硬编码的 `tensor.` 前缀分支就是被这条挡住的 —— 设计文档 04 的 Phase 1 想保留
// 一条无条件 `strings.HasPrefix(opcode,"tensor.") → dispatch.Compute` 兜底，
// 那会连用户自己起名叫 tensor 的库一起劫持。要恢复兜底就必须先判本地有没有定义。
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

// TestSelectFindsRegisteredBackend 是 Select 最基本的一条：注册且 ready 时选中。
//
// 一并钉住"按完整 opcode 匹配"：注册的是 fake.echo，拿裸 echo 去查必须找不到。
// 前一版按最后一个点拆成 (后端 fake, 算子 echo)，裸 echo 会扫中同一个后端 ——
// 换句话说这半条用例在旧模型下是反的。
func TestSelectFindsRegisteredBackend(t *testing.T) {
	kv := newKV(t)
	be := registerBackend(t, kv, "fake", "fake.echo")

	b, err := dispatch.Select(context.Background(), kv, "fake.echo")
	if err != nil || b != be {
		t.Fatalf("实得 (%q,%v)，期望 (%q,nil)", b, err, be)
	}
	if _, err := dispatch.Select(context.Background(), kv, "echo"); err == nil {
		t.Fatal("注册的是完整 opcode fake.echo，裸 echo 不该匹配")
	}
}

// TestSelectNoBackend 覆盖「一个后端都没注册该算子」。
func TestSelectNoBackend(t *testing.T) {
	kv := newKV(t)
	registerBackend(t, kv, "llm", "llm.complete")

	_, err := dispatch.Select(context.Background(), kv, "llm.chat")
	if err == nil {
		t.Fatal("没有后端支持 llm.chat 时必须报错")
	}
	if !strings.Contains(err.Error(), "no backend supports") {
		t.Fatalf("错误信息应指明没有后端支持，实得 %v", err)
	}
}

// TestSelectSkipsNotReadyBackend 确认候选里有不可用的后端时会继续找别的候选，
// 而不是锁定第一个候选后硬失败。offline 的 aaa 排在 zzz 前面（List 按插入序）。
func TestSelectSkipsNotReadyBackend(t *testing.T) {
	kv := newKV(t)
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp("aaa", "x.echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysRwirBackendStatus("aaa"), Val: kvspace.NewChar("offline")},
	})
	be := registerBackend(t, kv, "zzz", "x.echo")

	b, err := dispatch.Select(context.Background(), kv, "x.echo")
	if err != nil || b != be {
		t.Fatalf("应跳过 offline 的 aaa 选中 %s，实得 (%q,%v)", be, b, err)
	}
}

// TestVMPrimitivesBeatRegistry 钉住调度链顺序：**VM 原语永远赢过后端注册**。
//
// 这条不变量在 execute.go 与 router.go 的注释里被反复声明，却一直零测试覆盖 ——
// 把委托分支挪到 IsControlOp / IsNativeOp / isCopyOp 之上，全仓测试照样全绿。
// 注册表是外部进程自由写入的，能夺走这三类就等于把整个语言的语义交出去：
//   - 劫持 print：程序打印的每一个值都流向外部后端
//   - 劫持 =    ：后端拿到被赋的值与写槽绝对路径，写什么变量就是什么
//
// 每个 case 都注册一个真执行器（会把 input 原样回写），所以一旦劫持成功，
// 断言的不只是"任务数为 0"，还有程序结果被改写。
func TestVMPrimitivesBeatRegistry(t *testing.T) {
	for _, tc := range []struct{ name, opcode, src, wantKey, wantVal string }{
		{"内建 print", "print", `
rwfunc main() -> () {
	42 -> v
	print(v)
}

main()
`, "[0,0]/v", "42"},
		{"赋值 =", "=", `
rwfunc main() -> () {
	7 -> x
}

main()
`, "[0,0]/x", "7"},
		{"算术 +", "+", `
rwfunc main() -> () {
	3 + 4 -> y
}

main()
`, "[0,0]/y", "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := newKV(t)
			be := startBackend(t, kv, "evil", tc.opcode)
			loadSrc(t, kv, tc.src)
			vtid := run(t, kv, "init")
			be.stop()

			if ids := be.taskIDs(); len(ids) != 0 {
				t.Errorf("opcode %q 被后端劫持了，收到任务 %v", tc.opcode, ids)
			}
			if got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, tc.wantKey)); got.String() != tc.wantVal {
				t.Errorf("%s = %q，期望 %q —— 结果被劫持改写", tc.wantKey, got.String(), tc.wantVal)
			}
		})
	}
}

// TestAbsolutePathCallDoesNotPanic 是一条崩溃回归。
//
// `/lib/math.sum(3,4) -> s` 这种绝对路径调用形态（仓库自带
// tutorial/06-lib/cross/inline.kv 与 lib_b.kv 就这么写）让 opcode 带上斜杠。
// 委托判据把 opcode 原样拼进 /sys/rwir-backend/<b>/op/<opcode>，得到双斜杠，
// 而 art 后端对非规范路径是 **panic 而非报错**，kvlang 全仓零处 recover
// —— 整个 VM 带着 Go 栈当场死掉。
//
// 两个条件缺一都看不到，所以既有测试与 tutorial 全都漏了它：
//   - 注册表必须非空（循环体一次都不进就不会拼这个键）；offline 的记录也算，
//     因为 GetOne 发生在在岗判定之前
//   - 必须是 art://（redis 后端一处路径校验都没有，只会静默查不到）
func TestAbsolutePathCallDoesNotPanic(t *testing.T) {
	kv := newKV(t)
	// 注册表非空即可，这个后端与被调函数毫无关系
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp("llm", "llm.chat"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysRwirBackendStatus("llm"), Val: kvspace.NewChar("offline")},
	})
	loadSrc(t, kv, `
lib math { rwfunc sum(A:int64, B:int64) -> (C:int64) { A + B -> C } }

rwfunc main() -> () {
	/lib/math.sum(3, 4) -> s
}

main()
`)
	vtid := run(t, kv, "init") // 不 panic 即通过第一关
	if got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/s")); got.String() != "7" {
		t.Fatalf("绝对路径调用应正常本地执行：s=%q，期望 \"7\"", got.String())
	}
}

// TestBadOpcodeShapesAreNotDelegated 覆盖其余不能当路径段用的 opcode 形态。
// 每一条在加守卫前都会让 art 后端 panic。
func TestBadOpcodeShapesAreNotDelegated(t *testing.T) {
	kv := newKV(t)
	registerBackend(t, kv, "any", "any.op") // 让注册表非空
	for _, opcode := range []string{"/lib/math.sum", "..", ".", "x//y", "../evil", "", "a/b"} {
		if dispatch.IsDelegatedOp(kv, opcode) {
			t.Errorf("IsDelegatedOp(%q) 不该为真 —— 这种名字注册不上", opcode)
		}
		if _, err := dispatch.Select(context.Background(), kv, opcode); err == nil {
			t.Errorf("Select(%q) 应报错而不是 panic 或误选", opcode)
		}
	}
}

// TestSelectNoOnDutyBackend 覆盖「后端注册了该算子、但没有一个在岗」。
// 这条与「没人支持」必须报不同的错：前者是执行器挂了，后者是算子名拼错或忘了
// 装后端 —— 处置完全不同。
func TestSelectNoOnDutyBackend(t *testing.T) {
	kv := newKV(t)
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp("dead", "dead.echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysRwirBackendStatus("dead"), Val: kvspace.NewChar("offline")},
	})
	b, err := dispatch.Select(context.Background(), kv, "dead.echo")
	if err == nil {
		t.Fatalf("没有在岗后端时必须报错，实得 (%q,nil)", b)
	}
	if !strings.Contains(err.Error(), "no on-duty backend") {
		t.Fatalf("错误信息应区别于「没人支持」，实得 %v", err)
	}
}

// TestBusyBackendIsStillUsable 钉住 busy 算在岗。
//
// 文档 01 的 status 词表是 ready|busy|offline。把 busy 当不可用，会让一个短暂
// 繁忙的后端直接触发硬失败 —— 而它自报的 load 本来就足以让 Select 避开它。
func TestBusyBackendIsStillUsable(t *testing.T) {
	kv := newKV(t)
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp("b1", "busy.echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysRwirBackendStatus("b1"), Val: kvspace.NewChar("busy")},
	})
	if !dispatch.IsDelegatedOp(kv, "busy.echo") {
		t.Fatal("busy 后端应算在岗 → 仍判定为可委托")
	}
	if b, err := dispatch.Select(context.Background(), kv, "busy.echo"); err != nil || b != "b1" {
		t.Fatalf("busy 后端应可被选中，实得 (%q,%v)", b, err)
	}
}

// TestOfflineBackendFallsBackToLocal 是本轮最容易漏的一条语义。
//
// 文档 01 规定注销时只写 status=offline、**不删 op 子键**（记录留着排障）。
// 若 IsDelegatedOp 只看 op 子键存在性，一个永久死掉的后端会永久霸占该 opcode：
// 判定为可委托 → Select 选不出 → Delegate 直接 SetError 硬失败，既不回落本地、
// 也没有任何后端能接手。这里要求它回落到 /lib 里的同名 rwfunc 并真的算出结果。
//
// 同时钉住 IsDelegatedOp 与 Select 判定一致：不一致就会出现「说能委托、却选不出」
// 的空档。
func TestOfflineBackendFallsBackToLocal(t *testing.T) {
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
	// 注册后再下线：op 子键留着，只把 status 改成 offline
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp("gpu", "tensor.matmul"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysRwirBackendStatus("gpu"), Val: kvspace.NewChar("ready")},
	})
	if !dispatch.IsDelegatedOp(kv, "tensor.matmul") {
		t.Fatal("前置条件不成立：ready 时应判定为可委托")
	}
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendStatus("gpu"), Val: kvspace.NewChar("offline")},
	})

	if dispatch.IsDelegatedOp(kv, "tensor.matmul") {
		t.Fatal("后端下线后不该再判定为可委托 —— op 子键还在，但没人在岗")
	}
	vtid := run(t, kv, "init")
	if got := kvspace.GetOne(kv, keytree.VThreadAt(vtid, "[0,0]/r")); got.String() != "42" {
		t.Fatalf("后端下线应回落到 /lib 里的同名 rwfunc：r=%q，期望 \"42\"", got.String())
	}
}

// TestSelectMissingStatusIsNotReady 钉住「status 键缺失 ≠ ready」。
// 后端注册是多次写入，op 子键先落、status 后落是常态；这个窗口里必须当它不可用，
// 否则任务会投给一个还没准备好的进程。把 status 判定写成"非 offline 即可用"
// 就会在这里变红。
func TestSelectMissingStatusIsNotReady(t *testing.T) {
	kv := newKV(t)
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp("half", "half.echo"), Val: kvspace.NewChar("1")},
	}) // 只写了能力声明，没写 status

	if _, err := dispatch.Select(context.Background(), kv, "half.echo"); err == nil {
		t.Fatal("status 缺失的后端不该被选中")
	}
}

// TestSelectPicksLowestLoad 确认真的在按负载挑后端。
// 所有后端 load 都是 0 的话，比较逻辑（含 >= 与 > 之别、以及到底有没有记录
// 当前最优）完全不可观测。
func TestSelectPicksLowestLoad(t *testing.T) {
	kv := newKV(t)
	for _, tc := range []struct{ name, load string }{{"m7", "0.9"}, {"m8", "0.1"}, {"m9", "0.5"}} {
		kv.Set([]kvspace.KVPair{
			{Key: keytree.SysRwirBackendOp(tc.name, "m.echo"), Val: kvspace.NewChar("1")},
			{Key: keytree.SysRwirBackendStatus(tc.name), Val: kvspace.NewChar("ready")},
			{Key: keytree.SysRwirBackendLoad(tc.name), Val: kvspace.NewChar(tc.load)},
		})
	}
	b, err := dispatch.Select(context.Background(), kv, "m.echo")
	if err != nil || b != "m8" {
		t.Fatalf("应选中 load 最低的 m8，实得 (%q,%v)", b, err)
	}
}

// TestSelectMalformedLoadDoesNotWin 确认坏 load 不会**胜过**正常后端。
//
// 注册表是外部进程写的，半截/畸形数据是常态。而 0 是最低负载：把坏值当 0 参与
// 竞争，一个写坏了 load 的后端会吸走全部流量。
//
// 只判 strconv.ParseFloat 的 err 是不够的，下面三类都能解析成功却毁掉路由：
//   - "NaN"：NaN 与任何数比较都是 false ⇒ `load >= bestLoad` 恒不成立 ⇒ 每个
//     后端都覆盖前一个，「负载最低」无声退化成「最后注册的赢」。
//   - "-Inf" / "-5"：负值恒小于一切，该后端永远独占路由。
//   - "2"：超出 [0,1] 的自报值同样不可信。
//
// **两种注册顺序都要测**，否则测不出 NaN。负值的失效形态是"坏的恒赢"，坏后端
// 先注册就能抓到；而 NaN 的失效形态是"最后注册的赢"——坏后端排在前面时，正常
// 后端反而侥幸胜出，用例照绿。实测：只测一种顺序时，去掉 NaN 判定的变异存活。
func TestSelectMalformedLoadDoesNotWin(t *testing.T) {
	for _, bad := range []string{"not-a-number", "", "NaN", "Inf", "-Inf", "-5", "2", "1e400"} {
		for _, badFirst := range []bool{true, false} {
			order := "坏的先注册"
			if !badFirst {
				order = "坏的后注册"
			}
			t.Run("load="+bad+"/"+order, func(t *testing.T) {
				kv := newKV(t)
				op := "mix.echo"
				reg := func(name, load string) {
					kv.Set([]kvspace.KVPair{
						{Key: keytree.SysRwirBackendOp(name, op), Val: kvspace.NewChar("1")},
						{Key: keytree.SysRwirBackendStatus(name), Val: kvspace.NewChar("ready")},
						{Key: keytree.SysRwirBackendLoad(name), Val: kvspace.NewChar(load)},
					})
				}
				if badFirst {
					reg("bad", bad)
					reg("good", "0.5")
				} else {
					reg("good", "0.5")
					reg("bad", bad)
				}
				if b, err := dispatch.Select(context.Background(), kv, op); err != nil || b != "good" {
					t.Fatalf("load=%q（%s）的后端不该胜过 load=0.5 的正常后端：实得 (%q,%v)", bad, order, b, err)
				}
			})
		}
	}
}

// TestSelectAllLoadsMalformedStillPicksOne 确认坏 load 是「降级」不是「排除」：
// 全场只有坏后端时仍要选出一个，而不是报「无可用后端」。
func TestSelectAllLoadsMalformedStillPicksOne(t *testing.T) {
	kv := newKV(t)
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp("only", "solo.echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysRwirBackendStatus("only"), Val: kvspace.NewChar("ready")},
		{Key: keytree.SysRwirBackendLoad("only"), Val: kvspace.NewChar("NaN")},
	})
	if b, err := dispatch.Select(context.Background(), kv, "solo.echo"); err != nil || b != "only" {
		t.Fatalf("唯一后端即使 load 畸形也应被选中，实得 (%q,%v)", b, err)
	}
}

// TestSelectEqualLoadIsStable 钉住等负载时的取舍：先注册的胜出。
//
// 常见情形恰恰是"一堆后端都空闲（load 全缺省 0）"，此时 `>=` 与 `>` 的差别就是
// 全部结果 —— 前者先到先得，后者后来居上。两种都说得通，但必须钉死一个，
// 否则哪天有人"顺手"改了比较符，路由目标会整体漂移而没有任何测试变红。
// kv.List 返回插入顺序（实测三次调用结果一致），所以"先注册"是可控的。
func TestSelectEqualLoadIsStable(t *testing.T) {
	kv := newKV(t)
	// 分两次写入以保证顺序：先 t10，后 t11，均不写 load（缺省 0）
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp("t10", "tie.echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysRwirBackendStatus("t10"), Val: kvspace.NewChar("ready")},
	})
	kv.Set([]kvspace.KVPair{
		{Key: keytree.SysRwirBackendOp("t11", "tie.echo"), Val: kvspace.NewChar("1")},
		{Key: keytree.SysRwirBackendStatus("t11"), Val: kvspace.NewChar("ready")},
	})

	for i := 0; i < 3; i++ { // 顺带确认结果稳定，不随调用次数漂移
		b, err := dispatch.Select(context.Background(), kv, "tie.echo")
		if err != nil || b != "t10" {
			t.Fatalf("第 %d 次：等负载应选先注册的 t10，实得 (%q,%v)", i+1, b, err)
		}
	}
}
