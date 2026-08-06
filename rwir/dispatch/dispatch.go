// Package dispatch 负责把委托 rwir 派发给外部执行器。
//
// 委托 rwir = /lib 下 kind=rwir 的签名键（有签名、无指令体），由源码里的
// `rwir name(...) -> (...)` 声明产生。执行器在 /sys/op/<backend>/ 注册能力后
// 接管这些算子的求值，调用点形态与普通 rwfunc 完全一致。
//
// # 任务对象
//
//	/sys/task/<taskid>.status   pending | done | failed（持久键）
//	/sys/task/<taskid>.done     完成信号（Notify 目标，非持久键）
//
// 完成信号不复用 /vthread/<vtid>/‥status：那个键同时表示 vthread 终止，
// 且 SetDone/SetError 用同一套值词表 —— 执行器按 VM 自己的方式发一次信号，
// 就会把整个 vthread 静默终止掉。
//
// # 协议顺序
//
// 执行器必须**先写 outputs、再置 .status，最后才 Notify .done**。
// VM 侧 Watch 返回后会复查 .status，信号丢失不会被误判成功；反过来若先发信号
// 后写值，VM 可能在值落地前就继续执行。
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/array2d/kvspace-go"
	"kvlang/keytree"
	"kvlang/logx"
	"kvlang/rwir"
	"kvlang/rwir/builtin"
	"kvlang/vthread"
)

// defaultTimeout 委托调用的兜底超时。
//
// 是 var 不是 const —— 测试要覆盖超时路径就必须能调小它，否则每个用例都得等
// 30 秒，结果就是没人写这类测试（把 Watch 之后的 status 复查整段删掉，测试
// 照样全绿）。SetTimeoutForTest 是唯一的改写入口。
//
// TODO: 拆成两个，照 Temporal 的做法 —— StartToClose 界定合法总时长、
// HeartbeatTimeout 界定沉默时长，均从 /sys/op/<b>/func/<op> 按算子读。
// 一个 2 小时的任务配 30 秒心跳超时，检测 worker 死亡只要 30 秒而不是 2 小时。
var defaultTimeout = 30 * time.Second

// ParamRef 描述一个跨界参数。
// 读参给值（执行器不必自己解 XValue 的 TLV 编码），写参给绝对路径。
// 二者均经 ‥rparam/‥wparam 零拷贝重定向解析，与 VM 自身的读写完全同路。
type ParamRef struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

// OpTask 是投递给执行器的任务描述，JSON 编码后推进 /sys/op/<b>/<n>/cmd。
type OpTask struct {
	ID      string     `json:"id"`
	Vtid    string     `json:"vtid"`
	PC      string     `json:"pc"`
	Opcode  string     `json:"opcode"`
	Inputs  []ParamRef `json:"inputs"`
	Outputs []ParamRef `json:"outputs"`
	DoneKey string     `json:"done_key"`
}

// buildTask 解析读写槽，构造 OpTask。
//
// 走 builtin 的解析函数而非自行拼路径 —— 与 VM 自身完全相同的路径，因此遵守
// ‥rparam/‥wparam 零拷贝重定向，嵌套 rwfunc 帧内的局部变量也指向真实位置。
// 旧实现用 keytree.VThreadAt(vtid, name) 拼到 vthread 根帧，只在顶层帧且配
// 绝对路径时才碰巧正确。
func buildTask(kv kvspace.KVSpace, taskID, vtid, pc string, inst *rwir.Rwir) (*OpTask, error) {
	framePath := keytree.FrameRoot(pc)
	task := &OpTask{
		ID:      taskID,
		Vtid:    vtid,
		PC:      pc,
		Opcode:  inst.Opcode,
		DoneKey: keytree.SysTask(taskID, "done"),
	}
	for _, r := range inst.Reads {
		task.Inputs = append(task.Inputs, ParamRef{Value: builtin.ResolveReadValue(kv, framePath, r).String()})
	}
	for _, w := range inst.Writes {
		key, err := builtin.ResolveWriteSlot(kv, framePath, w.Name)
		if err != nil {
			// 落点校验必须在派发**之前**：outputs 一旦随任务出了门，写就发生在
			// VM 之外，再也拦不住了。
			return nil, err
		}
		task.Outputs = append(task.Outputs, ParamRef{Key: key})
	}
	return task, nil
}

// Delegate 把一条委托 rwir 派发给外部执行器并等待完成。
//
// 任何失败都必须写 vthread 错误状态：cmd/kvlang/run.go 的 reportRunError 靠
// ‥error/msg 决定退出码，只 return error 的话会变成静默失败且 exit 0。
func Delegate(ctx context.Context, kv kvspace.KVSpace, vtid, pc string, inst *rwir.Rwir) error {
	fail := func(format string, a ...any) error {
		msg := "RuntimeError: delegate: " + fmt.Sprintf(format, a...)
		vthread.SetError(ctx, kv, vtid, pc, msg)
		return fmt.Errorf("%s", msg)
	}
	// failAs 原样保留下层的错误类名。仓库按诊断的首个 token 归类
	// （README 的 Error Cases 一节、tutorial/error_cases/<类名>/ 的目录布局都靠它），
	// 把 PermissionError 裹进 "RuntimeError: delegate: …" 会让同一个违规在原生写
	// 与委托写两条路上被归成两类。
	failAs := func(err error) error {
		vthread.SetError(ctx, kv, vtid, pc, err.Error())
		return err
	}

	// 调用点与声明的读写参个数必须一致。委托调用不经 HandleCall，没人按声明
	// 绑定参数，少了这道检查就会把畸形任务静默发给外部系统：签名声明 1 个
	// 读参、调用点给 3 个，任务照发、多出来的两个悄悄跟着走，exit 0。
	if sig, ok := delegatedSig(kv, inst.Opcode); ok {
		if r, ok2 := sig.(kvspace.Rwir); ok2 {
			if got, want := len(inst.Reads), int(r.NumReads()); got != want {
				return fail("%s: 读参个数不符：调用点 %d 个，声明 %d 个（%s）", inst.Opcode, got, want, r.Sig())
			}
			if got, want := len(inst.Writes), int(r.NumWrites()); got != want {
				return fail("%s: 写参个数不符：调用点 %d 个，声明 %d 个（%s）", inst.Opcode, got, want, r.Sig())
			}
		}
	}

	backend, n, err := Select(ctx, kv, inst.Opcode)
	if err != nil {
		return fail("%v", err)
	}

	// taskID 必须带自增序号：scope 帧每轮循环复用同一 PC，仅靠 (vtid, pc) 会
	// 每轮撞号；而 Notify 队列是持久的，撞号会让这一轮读走上一轮迟到的信号。
	//
	// 这个唯一性**只在单进程内成立**，别指望更多：
	//   - vtid 由 vthread.AllocVtid 分配，底层 NextSeq 是 Get-then-Set，不原子。
	//     两个 kvlang 进程连同一个 redis 并发启动会拿到同一个 vtid，taskID 随之
	//     撞号。实测两进程 4/4 撞号，且不委托的程序同样撞 —— 根因是既有的
	//     AllocVtid，不是这里，但本函数的唯一性论证确实建立在它之上。
	//   - 两个计数器都在 /vthread 下，`kvspace deltree /vthread` 会把 ‥seq 一起
	//     清零，于是新一轮从头编号，可能复用上一轮遗留任务的 ID。
	// 要真正封死得换成不可复用的 ID（随机数 / 进程实例标识），那会改变
	// taskID 的可预测性，留给后续决定。
	taskID := fmt.Sprintf("%s-%d", vtid, vthread.NextSeq(kv, keytree.VThreadDelegSeq(vtid)))
	task, err := buildTask(kv, taskID, vtid, pc, inst)
	if err != nil {
		// 落点校验失败：保留 PermissionError 类名，别改写成 RuntimeError
		return failAs(err)
	}
	specJSON, err := json.Marshal(task)
	if err != nil {
		return fail("%s: marshal task: %v", inst.Opcode, err)
	}

	statusKey := keytree.SysTask(taskID, "status")
	if err := kv.Set([]kvspace.KVPair{{Key: statusKey, Val: kvspace.NewChar("pending")}}); err != nil {
		return fail("%s: init task status: %v", inst.Opcode, err)
	}

	// 派发前清空写槽。看着多余，其实是下面那道"输出真的被写了吗"检查的前提：
	// 循环里每轮写同一个槽，不清的话第 2 轮即便执行器什么都没写，槽里也还留着
	// 第 1 轮的值，检查就形同虚设。清空发生在 Notify 之前，执行器不可能看到
	// 任务、也就不可能与这次删除竞争。
	for _, out := range task.Outputs {
		kv.Del(out.Key)
	}

	// NewChar 而非 NewBytes —— NewBytes 给每个元素追加一个 NUL，JSON 尾部带
	// \x00 会让执行器的解析器报出完全指不到病因的错。
	cmdQueue := keytree.SysOpCmd(backend, n)
	if err := kv.Notify(cmdQueue, kvspace.NewChar(string(specJSON))); err != nil {
		return fail("push task: %v", err)
	}
	logx.Debug("[%s] DELEGATE %s task=%s → %s", vtid, inst.Opcode, taskID, cmdQueue)

	vthread.Set(ctx, kv, vtid, pc, "wait")

	// Watch 之后仍复查持久 status。
	//
	// KVSpace 接口注释把 Notify 写成「投递一次性通知信号」，但两个后端的实现
	// 都是持久队列（art 是内存切片，redis 是 LPUSH/BLPOP 列表），执行器早于
	// VM 发信号也不会丢。只信信号 = 依赖未写进接口的实现行为；只信状态 =
	// 无法及时唤醒。两个都用：信号负责唤醒，状态负责判定。
	kv.Watch(task.DoneKey, defaultTimeout)
	if st := kvspace.GetOne(kv, statusKey); st.String() != "done" {
		return fail("%s: timeout or failed (status=%q)", inst.Opcode, st.String())
	}

	// 报了 done 还不够：执行器不可信，它可能只发信号不写值。少了这道检查，
	// VM 会拿着空值继续跑并 exit 0 —— 静默错误结果，比崩掉难查得多。
	for _, out := range task.Outputs {
		if kvspace.IsNone(kvspace.GetOne(kv, out.Key)) {
			return fail("%s: 执行器报 done 但没写输出槽 %s", inst.Opcode, out.Key)
		}
	}

	logx.Debug("[%s] DONE %s task=%s", vtid, inst.Opcode, taskID)
	kv.Del(statusKey) // 成功即回收；失败路径留着给人查
	vthread.Set(ctx, kv, vtid, rwir.NextPC(pc), "running")
	return nil
}
