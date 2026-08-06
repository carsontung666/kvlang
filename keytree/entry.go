package keytree

import "strings"

const LibRoot = PathSegSep + PathSegLib

// FuncKey 把调用点的算子名解析为 /lib 下的签名键。
// 纯路径运算，不访问 kvspace —— layout.HandleCall 与 rwir/dispatch 共用同一规则。
//
//	"add"           → /lib/add
//	"mylib.add"     → /lib/mylib.add
//	"llm.chat"      → /lib/llm.chat        （按最后一个点拆 pkg/name，再由 LibFunc 拼回）
//	"/lib/mylib.add"→ /lib/mylib.add
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
