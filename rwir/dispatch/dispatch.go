// Package dispatch 负责把委托 rwir 派发给外部执行器。
//
// 委托 rwir = /lib 下 kind=rwir 的签名键（有签名、无指令体），由 `rwir name(...) -> (...)`
// 声明产生。执行器在 /sys/op/<backend>/ 注册能力后接管这些算子的求值。
//
// # 任务对象
//
//	/sys/task/<taskid>.status   pending | done | failed
//	/sys/task/<taskid>.done     完成信号（Notify 目标，非持久键）
//
// 完成信号不能复用 /vthread/<vtid>/‥status：那个键同时表示 vthread 终止，
// 且 SetDone/SetError 用同一套值词表 —— 执行器按 VM 自己的方式发信号会静默
// 终止整个 vthread。
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
const defaultTimeout = 30 * time.Second

// ParamRef 描述一个跨界参数。读参给值（执行器不必自己解 XValue 的 TLV 编码），
// 写参给绝对路径。二者均经 ‥rparam/‥wparam 零拷贝重定向解析。
type ParamRef struct {
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

// buildTask 解析读写槽，构造 OpTask。
//
// 走 builtin 的解析函数而非自行拼路径 —— 与 VM 自身完全相同的路径，因此遵守
// ‥rparam/‥wparam 零拷贝重定向，嵌套 rwfunc 帧内的局部变量也指向真实位置。
func buildTask(kv kvspace.KVSpace, taskID, vtid, pc string, inst *rwir.Rwir) *OpTask {
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
		task.Outputs = append(task.Outputs, ParamRef{Key: builtin.ResolveWriteKey(kv, framePath, w.Name)})
	}
	return task
}

// Delegate 把一条委托 rwir 派发给外部执行器并等待完成。
func Delegate(ctx context.Context, kv kvspace.KVSpace, vtid, pc string, inst *rwir.Rwir) error {
	fail := func(format string, a ...any) error {
		msg := "RuntimeError: delegate: " + fmt.Sprintf(format, a...)
		vthread.SetError(ctx, kv, vtid, pc, msg)
		return fmt.Errorf("%s", msg)
	}

	backend, n, err := Select(ctx, kv, inst.Opcode)
	if err != nil {
		return fail("%v", err)
	}

	// taskID 必须带序号：scope 帧每轮循环复用同一 PC，仅靠 (vtid, pc) 会碰撞，
	// 叠加持久化的 Notify 队列会让下一轮读走上一轮迟到的完成信号。
	taskID := fmt.Sprintf("%s-%d", vtid, vthread.NextSeq(kv, keytree.VThreadDelegSeq(vtid)))
	task := buildTask(kv, taskID, vtid, pc, inst)
	specJSON, _ := json.Marshal(task) // OpTask 全是 string 字段，不可能失败

	statusKey := keytree.SysTask(taskID, "status")
	kv.Set([]kvspace.KVPair{{Key: statusKey, Val: kvspace.NewChar("pending")}})

	// NewChar 而非 NewBytes —— 后者给每个元素追加一个 NUL，JSON 尾部带 \x00
	// 会让执行器的解析器报出完全指不到病因的错。
	cmdQueue := keytree.SysOpCmd(backend, n)
	if err := kv.Notify(cmdQueue, kvspace.NewChar(string(specJSON))); err != nil {
		return fail("push task: %v", err)
	}
	logx.Debug("[%s] DELEGATE %s task=%s → %s", vtid, inst.Opcode, taskID, cmdQueue)

	vthread.Set(ctx, kv, vtid, pc, "wait")

	// Watch 返回后仍复查持久 status：接口注释把 Notify 描述成 fire-and-forget，
	// 实现却是持久队列，依赖实现行为的代码将来会坏。
	kv.Watch(task.DoneKey, defaultTimeout)
	if st := kvspace.GetOne(kv, statusKey); st.String() != "done" {
		return fail("%s: timeout or failed (status=%q)", inst.Opcode, st.String())
	}

	logx.Debug("[%s] DONE %s task=%s", vtid, inst.Opcode, taskID)
	kv.Del(statusKey) // 成功即回收；失败路径留着给人查
	vthread.Set(ctx, kv, vtid, rwir.NextPC(pc), "running")
	return nil
}
