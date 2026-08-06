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
