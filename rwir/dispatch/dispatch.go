// Package dispatch 负责把委托 rwir 派发给外部执行器。
//
// 委托 rwir = /lib 下 kind=rwir 的签名键（有签名、无指令体），由 `rwir name(...) -> (...)`
// 声明产生。执行器在 /sys/op/<backend>/ 注册能力后接管这些算子的求值。
//
// # 任务对象（点号键族，不是斜杠子项）
//
// 之所以用点号：kvlang 里 h.field 降级为 at(h,"field")，键形态是 base+"."+name
// （keytree.Member）。用斜杠的话 handle 解引用就得新加机制，"零新类型"不成立。
//
//	/sys/task/<taskid>.status     pending | running | done | failed | declined | refused
//	/sys/task/<taskid>.opcode
//	/sys/task/<taskid>.vtid       回调坐标
//	/sys/task/<taskid>.pc
//	/sys/task/<taskid>.spec       OpTask JSON
//	/sys/task/<taskid>.done       完成信号键（Notify 目标）
//
// # 为什么完成信号不能复用 /vthread/<vtid>/‥status
//
// 那个键同时表示 vthread 终止，且 SetDone/SetError 用的是同一套值词表。
// 执行器若按 VM 自己的方式发信号（Del + Notify），会静默终止整个 vthread。
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/array2d/kvspace-go"
	"kvlang/keytree"
	"kvlang/logx"
	"kvlang/rwir"
	"kvlang/rwir/builtin"
	"kvlang/vthread"
)

// defaultTimeout 委托调用的兜底超时。
// TODO(阶段2): 改为 per-op 从 /sys/op/<b>/func/<op> 读，并拆成
// StartToClose（合法时长）与 HeartbeatTimeout（沉默时长）两个。
const defaultTimeout = 30 * time.Second

// TaskRoot 任务对象根。
const TaskRoot = keytree.PathSegSep + "sys" + keytree.PathSegSep + "task"

// ParamRef 描述一个跨界参数。
//
//	Name  槽位在源码里的写法（`x`、`/abs/path`、字面量）。仅供诊断。
//	Key   绝对 kvspace 路径，执行器可直接读/写。读参若是字面量则为空。
//	Value 读参的当前值，由 VM 解析好 —— 执行器不必自己解 XValue 的 TLV 编码。
//	      写参不带 Value（还没算出来）。
//
// Key 与 Value 都经 ‥rparam/‥wparam 零拷贝重定向解析，所以嵌套 rwfunc 帧内的
// 局部变量也指向真实位置。
type ParamRef struct {
	Name  string `json:"name"`
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

// OpTask 是投递给执行器的任务描述。
type OpTask struct {
	ID      string     `json:"id"`
	Vtid    string     `json:"vtid"`
	PC      string     `json:"pc"`
	Opcode  string     `json:"opcode"`
	Inputs  []ParamRef `json:"inputs"`
	Outputs []ParamRef `json:"outputs"`
	DoneKey string     `json:"done_key"`
}

// taskField 返回任务对象的成员键。
func taskField(taskID, field string) string {
	return keytree.Member(TaskRoot+keytree.PathSegSep+taskID, field)
}

// nextDelegSeq 原子性不足的自增序列（单 VM 进程内够用）。
//
// 必需：taskID 不能只由 (vtid, pc) 决定 —— scope 帧在每轮循环复用同一 PC
// （layout.HandleScope），while 里的委托每轮会算出同一个 taskID。叠加 Notify
// 的持久 LIFO 队列，上一轮迟到的 .done 会被下一轮的 Watch 立刻读走。
func nextDelegSeq(kv kvspace.KVSpace, vtid string) int64 {
	key := keytree.VThreadAt(vtid, keytree.RuntimeMemberSep+"delegseq")
	n, _ := strconv.ParseInt(kvspace.GetOne(kv, key).String(), 10, 64)
	n++
	kv.Set([]kvspace.KVPair{{Key: key, Val: kvspace.NewChar(strconv.FormatInt(n, 10))}})
	return n
}

func makeTaskID(kv kvspace.KVSpace, vtid string) string {
	return fmt.Sprintf("%s-%d", vtid, nextDelegSeq(kv, vtid))
}

// taskFields 返回一个任务对象的全部成员键。
// 点号键族没有目录，DelTree 抓不到这些兄弟键，清理必须逐个 Del。
func taskFields(taskID string) []string {
	segs := []string{"status", "opcode", "vtid", "pc", "spec", "done"}
	keys := make([]string, 0, len(segs))
	for _, s := range segs {
		keys = append(keys, taskField(taskID, s))
	}
	return keys
}

// buildTask 解析读写槽，构造 OpTask。
//
// 读参走 builtin.ResolveReadValue、写参走 builtin.ResolveWriteKey —— 与 VM 自身
// 完全相同的解析路径，因此遵守 ‥rparam/‥wparam 零拷贝重定向，也能正确处理
// 嵌套 rwfunc 帧内的局部变量。旧实现用 keytree.VThreadAt 拼到 vthread 根帧，
// 只在顶层配绝对路径时才碰巧正确。
func buildTask(kv kvspace.KVSpace, taskID, vtid, pc string, inst *rwir.Rwir) *OpTask {
	framePath := keytree.FrameRoot(pc)
	task := &OpTask{
		ID:      taskID,
		Vtid:    vtid,
		PC:      pc,
		Opcode:  inst.Opcode,
		DoneKey: taskField(taskID, "done"),
	}
	for _, r := range inst.Reads {
		task.Inputs = append(task.Inputs, ParamRef{
			Name:  r.Name,
			Key:   builtin.ResolveReadKey(kv, framePath, r),
			Value: builtin.ResolveReadValue(kv, framePath, r).String(),
		})
	}
	for _, w := range inst.Writes {
		task.Outputs = append(task.Outputs, ParamRef{
			Name: w.Name,
			Key:  builtin.ResolveWriteKey(kv, framePath, w.Name),
		})
	}
	return task
}

// Delegate 把一条委托 rwir 派发给外部执行器并等待完成。
//
// 全程 recover：委托边界两侧的键都不完全受 VM 控制 —— 写槽名来自用户（可能是
// agent 生成的）代码，任务键里嵌了 vtid/pc。而 kvspace 对畸形键（如含 //）是
// panic 而非返回 error。委托的前提就是外部输入不该能掀掉 VM，所以在这里兜住，
// 转成正常的 vthread 错误。
func Delegate(ctx context.Context, kv kvspace.KVSpace, vtid, pc string, inst *rwir.Rwir) (err error) {
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("RuntimeError: delegate %s: panic: %v", inst.Opcode, r)
			vthread.SetError(ctx, kv, vtid, pc, msg)
			err = fmt.Errorf("%s", msg)
		}
	}()

	backend, n, err := Select(ctx, kv, inst.Opcode)
	if err != nil {
		msg := "RuntimeError: delegate: " + err.Error()
		// 旧实现只把 error 返回给 Execute，从不写 vthread 状态，
		// 于是 run.go 的 reportRunError 读不到 ‥error/msg → 静默失败且 exit 0。
		vthread.SetError(ctx, kv, vtid, pc, msg)
		return fmt.Errorf("%s", msg)
	}

	taskID := makeTaskID(kv, vtid)
	task := buildTask(kv, taskID, vtid, pc, inst)
	specJSON, err := json.Marshal(task)
	if err != nil {
		msg := "RuntimeError: delegate: marshal task: " + err.Error()
		vthread.SetError(ctx, kv, vtid, pc, msg)
		return fmt.Errorf("%s", msg)
	}

	// 状态在先，通知在后：持久状态必须先落地，Notify 只是提示。
	// 一次写全所有字段 —— at() 命中缺失键会 SetError 终止 vthread，
	// 所以 agent.poll(h) 早于后端写入时也必须读得到 .status。
	kv.Set([]kvspace.KVPair{
		{Key: taskField(taskID, "status"), Val: kvspace.NewChar("pending")},
		{Key: taskField(taskID, "opcode"), Val: kvspace.NewChar(inst.Opcode)},
		{Key: taskField(taskID, "vtid"), Val: kvspace.NewChar(vtid)},
		{Key: taskField(taskID, "pc"), Val: kvspace.NewChar(pc)},
		// NewChar 而非 NewBytes：NewBytes 会给每个元素追加一个 NUL，
		// JSON 尾部带 \x00 会让执行器的解析器报出完全指不到病因的错。
		{Key: taskField(taskID, "spec"), Val: kvspace.NewChar(string(specJSON))},
	})

	cmdQueue := keytree.SysOpCmd(backend, n)
	if err := kv.Notify(cmdQueue, kvspace.NewChar(string(specJSON))); err != nil {
		msg := "RuntimeError: delegate: push task: " + err.Error()
		vthread.SetError(ctx, kv, vtid, pc, msg)
		return fmt.Errorf("%s", msg)
	}
	logx.Debug("[%s] DELEGATE %s task=%s → %s", vtid, inst.Opcode, taskID, cmdQueue)

	vthread.Set(ctx, kv, vtid, pc, "wait")

	// 完成信号走 per-task 键，绝不碰 ‥status。
	// Watch 返回后仍复查持久 .status：接口注释把 Notify 描述成 fire-and-forget
	// （实现目前是持久队列），依赖当前实现行为的代码将来会坏。
	kv.Watch(task.DoneKey, defaultTimeout)
	if st := kvspace.GetOne(kv, taskField(taskID, "status")); st.String() != "done" {
		msg := fmt.Sprintf("RuntimeError: delegate %s: timeout or failed (status=%q)", inst.Opcode, st.String())
		vthread.SetError(ctx, kv, vtid, pc, msg)
		return fmt.Errorf("%s", msg)
	}

	logx.Debug("[%s] DONE %s task=%s", vtid, inst.Opcode, taskID)

	// 成功路径回收任务对象：结果已写进调用方的写槽，任务对象本身没有留存价值。
	// 失败路径**不**回收 —— 留着给人查。
	// TODO(阶段5): 加 durable await 后，回收要挪到 resumer 里，因为那时
	// 完成与恢复可能发生在不同进程。
	for _, k := range taskFields(taskID) {
		kv.Del(k)
	}

	vthread.Set(ctx, kv, vtid, rwir.NextPC(pc), "running")
	return nil
}
