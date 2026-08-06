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
	"strings"
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

// checkWriteKey 校验一个委托写槽的落点。
//
// 委托把写权限交到了 VM 之外：执行器拿到 outputs 里的绝对路径就直接写。
// 而 kvlang 的写参声明本身就是能力声明 —— `rwir f(a) -> (b)` 说的是「只写 b」，
// 原生 rwir 由 VM 构造性保证，委托 rwir 只能靠这里把关。
//
// 允许：调用方自己的 vthread 子树、用户全局键（根下的普通路径）。
// 拒绝（实测均可被外部执行器利用）：
//   - 末段以 ‥ 打头的引擎保留键 —— 写 ‥returnpc 可劫持控制流，写非 PC 值直接
//     让下一轮 Execute 裸 panic；写 ‥ro 可关掉只读参防线
//   - /lib —— 改写运行中的代码（VM 每次取指都重新从 KV 解码）
//   - /sys —— 篡改后端注册表、伪造别人的任务状态
//   - /dev —— device 层会把 detail 当文件路径 os.OpenFile(O_APPEND|O_CREATE)，
//     或当 WebSocket URL 直连，等于任意文件写与 SSRF
//   - 别的 vthread 子树
func checkWriteKey(vtid, key string) error {
	if key == "" || key[0] != '/' {
		return fmt.Errorf("写槽不是绝对路径: %q", key)
	}
	for _, seg := range strings.Split(key, keytree.PathSegSep) {
		if strings.HasPrefix(seg, keytree.RuntimeMemberSep) {
			return fmt.Errorf("写槽指向引擎保留键: %q", key)
		}
	}
	for _, deny := range []string{keytree.LibRoot, keytree.SysRoot, keytree.DevRoot} {
		if key == deny || strings.HasPrefix(key, deny+keytree.PathSegSep) {
			return fmt.Errorf("写槽指向受保护域 %s: %q", deny, key)
		}
	}
	if strings.HasPrefix(key, keytree.VthreadRoot+keytree.PathSegSep) {
		own := keytree.VThread(vtid) + keytree.PathSegSep
		if !strings.HasPrefix(key, own) {
			return fmt.Errorf("写槽指向其它 vthread: %q", key)
		}
	}
	return nil
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
	// 派发之前把关：一旦 outputs 出了门，写就发生在 VM 之外。
	for _, out := range task.Outputs {
		if err := checkWriteKey(vtid, out.Key); err != nil {
			return fail("%s: %v", inst.Opcode, err)
		}
	}
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
