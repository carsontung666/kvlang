package keytree

import "strings"

const LibRoot = PathSegSep + PathSegLib

// FuncKey 把调用点的算子名解析为 /lib 下的签名键。
// 纯路径运算，不访问 kvspace —— layout.HandleCall、layout.Bootstrap 与
// rwir/dispatch 共用同一规则。
//
//	"add"            → /lib/add
//	"mylib.add"      → /lib/mylib.add
//	"llm.chat"       → /lib/llm.chat
//	"a.b.c"          → /lib/a.b.c
//	"/lib/mylib.add" → /lib/mylib.add
//
// 实现上先按最后一个点拆成 pkg/name 再由 LibFunc 拼回。这个拆分点**不影响结果**
// ——拆开再用同一个分隔符拼回是恒等变换，用第一个点拆输出完全相同（已对含多个
// 点、前导点、尾随点、连续点、/lib 前缀等 11 种输入逐一比对确认）。保留 pkg/name
// 的形式只是为了和 LibFunc 的调用约定对齐；别据此以为这里的切分规则是语义的一部分。
// 委托路由不再拆点：后端注册表 /sys/rwir-backend/<b>/op/<opcode> 存的是完整
// opcode，判据拿调用点的 opcode 直接查（见 dispatch.backendsFor）。
func FuncKey(funcName string) string {
	var pkg string
	libPrefix := LibRoot + PathSegSep
	if strings.HasPrefix(funcName, libPrefix) {
		rest := funcName[len(libPrefix):]
		if dot := strings.LastIndex(rest, MemberSep); dot > 0 {
			pkg, funcName = rest[:dot], rest[dot+len(MemberSep):]
		} else {
			funcName = rest
		}
	} else if dot := strings.LastIndex(funcName, MemberSep); dot > 0 {
		pkg, funcName = funcName[:dot], funcName[dot+len(MemberSep):]
	}
	return LibFunc(pkg, funcName)
}

func LibFunc(pkg, name string) string {
	if pkg == "" { return LibRoot + PathSegSep + name }
	return LibRoot + PathSegSep + pkg + MemberSep + name
}

func LibSrc(pkg, name string) string {
	if pkg == "" { return LibRoot + PathSegSep + name + SrcExt }
	return LibRoot + PathSegSep + pkg + MemberSep + name + SrcExt
}
