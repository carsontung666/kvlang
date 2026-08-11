package keytree

const SysRoot = PathSegSep + SegSys

func SysOp(backend, n string) string { return SysRoot + PathSegSep + SegOp + PathSegSep + backend + PathSegSep + n }

func SysOpCmd(backend, n string) string { return SysOp(backend, n) + PathSegSep + SegCmd }

func SysOpFunc(backend, name string) string { return SysRoot + PathSegSep + SegOp + PathSegSep + backend + PathSegSep + SegFunc + PathSegSep + name }

const SysOpRoot   = PathSegSep + SegSys + PathSegSep + SegOp
const SysRwirRoot = PathSegSep + SegSys + PathSegSep + SegRwir

// SysTaskRoot 是委托任务对象的根目录。
const SysTaskRoot = PathSegSep + SegSys + PathSegSep + SegTask

// SysTask 返回委托任务对象的成员键：/sys/task/<id>.<field>。
// 点号键族而非斜杠子项 —— 与 kvlang 的 struct ≡ dict ≡ 键族约定一致，
// handle 可用现有的 at(h,"field") 解引用。
func SysTask(taskID, field string) string {
	return Member(SysTaskRoot+PathSegSep+taskID, field)
}

func SysRwir(opcode string) string { return SysRwirRoot + PathSegSep + opcode }

// ── 委托后端注册表 ────────────────────────────────────────────────────────
//
// /sys/rwir-backend/<name>/
//	├── status               ready | busy | offline —— 只有 ready/busy 算在岗
//	├── load                 自报负载，缺省 0；不可比较的值按满载
//	├── op/<opcode>          能力声明，**存在即支持**；这张表就是委托判据
//	└── cmd                  命令队列（Notify 目标）
//
// 文档 01 还规定了 heartbeat（每 5s 更新、15s 超时回写 offline）与
// category/<cat>（决定超时档位），**两者都尚未实现**：没有任何组件写或读它们，
// 所以死掉的后端不会被自动标成 offline，超时也还是单一常量。别照着这段注释
// 以为它们已经能用。
//
// 与 /sys/op/ 的分工：/sys/op/ 是 deepx op-plat 的遗留两级路由
// （backend + instance），本表是一级——后端名即实例名。委托判据只看本表。
//
// op 子键存的是**完整 opcode**（tensor.matmul、llm.chat），不剥命名空间：
// 判据要拿调用点的 opcode 直接查，剥了就得两边约定同一套拆分规则。
const SysRwirBackendRoot = PathSegSep + SegSys + PathSegSep + SegRwirBackend

func SysRwirBackend(name string) string { return SysRwirBackendRoot + PathSegSep + name }

func SysRwirBackendCmd(name string) string { return SysRwirBackend(name) + PathSegSep + SegCmd }

func SysRwirBackendStatus(name string) string { return SysRwirBackend(name) + PathSegSep + SegStatus }

func SysRwirBackendLoad(name string) string { return SysRwirBackend(name) + PathSegSep + SegLoad }

func SysRwirBackendOp(name, opcode string) string {
	return SysRwirBackend(name) + PathSegSep + SegOp + PathSegSep + opcode
}

// DoneRoot 是完成信号根，独立于 /sys 的一个域，整域归引擎（见 CheckWriteKey）。
//
// 它下面**没有持久树键**：DoneRwir 只出现在 Notify / Watch 的目标位置，而
// Notify 队列活在 kvspace 树之外。程序被禁止写这里是命名空间归属与纵深防御，
// 不是在堵一条可用的伪造路径 —— 树键写进去也唤不醒任何 Watch。
const DoneRoot = PathSegSep + SegDone

const DoneRwirRoot = DoneRoot + PathSegSep + SegRwir

func DoneRwir(requestID string) string { return DoneRwirRoot + PathSegSep + requestID }
