package keytree

import (
	"fmt"
	"strings"
)

const LibRoot = PathSegSep + PathSegLib

// CheckWriteKey 校验一条写指令的落点，原生与委托两条路共用。
//
// # 威胁模型（先看这个，否则会高估它）
//
// 三个角色，可信程度不同：
//
//	VM 与 kvspace   可信
//	kvlang 程序     半可信 —— 本项目的目标就是让 agent 写 kvlang 代码
//	外部执行器      不可信 —— LLM / agent sidecar
//
// **本函数是「程序沙箱」，不是「执行器沙箱」。**
// 它保证：agent 写出的 kvlang 代码，无论走原生写还是走委托，都改不动引擎状态、
// 改不了运行中的代码、动不了别的 vthread、碰不到 /dev 那个任意文件写的 sink。
// 这正是自进化场景需要的性质 —— agent 生成的代码进 /lib/gen/ 跑起来无法提权。
//
// 它不保证：不守协议的执行器被挡住。任务描述里带着 vtid 与 pc，执行器完全可以
// 自己推出任何键直接写（它按协议本来就在写 /sys/task/<id>.status）。封死那一侧
// 要靠 kvspace 的作用域凭证或居中 broker，不是这里。
//
// # 为什么只管写不管读
//
// 读侧刻意保持开放。委托出去的 agent 能读到指令集（/sys/rwir/）、源码（/lib/）、
// 活的调用栈、历史 vthread —— 这是 kvlang 相对「LLM SDK 绑定」的全部差异所在，
// 也是 article/self-evolving-robot.md 里自分析那一步的前提。限制读会直接砍掉
// 项目的核心论点。代价是程序可以把任意键的值送给执行器；在「程序半可信、
// 执行器按部署可信」这个模型下这是可接受的，换模型时必须重新评估。
//
// 允许：调用方自己的 vthread 子树、用户全局键（不在四个域根下的路径）。
// 拒绝：
//   - 非规范路径（空段 / . / ..）—— 纯前缀比较对它们无效，而 kvspace 当前是
//     「拒绝非规范路径」而非「规范化后再用」，一旦改成后者防线立刻失效
//   - 任一路径段以 ‥ 打头的引擎保留键 —— 写 ‥returnpc 可劫持控制流，写非 PC 值让
//     下一轮取指裸 panic；写 ‥ro 可关掉只读参防线
//   - /lib —— 改写运行中的代码（每次取指都重新从 KV 解码，注入立即生效）
//   - /sys —— 篡改后端注册表、伪造任务状态
//   - /dev —— device 层把值当文件路径 os.OpenFile(O_APPEND|O_CREATE) 或当
//     WebSocket URL 直连，等于任意文件写与 SSRF
//   - 别的 vthread 子树
//
// 已知边界：校验的是**未解链**的字面路径，而 kvspace 的 Set 会跟随 link。
// 自己子树里若存在指向 /lib 的 link，可穿透（当前无内建暴露 Link）。
func CheckWriteKey(vtid, key string) error {
	if key == "" || key[0] != PathSegSep[0] {
		return fmt.Errorf("写槽不是绝对路径: %q", key)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(key, PathSegSep), PathSegSep) {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return fmt.Errorf("写槽不是规范路径: %q", key)
		case strings.HasPrefix(seg, RuntimeMemberSep):
			return fmt.Errorf("写槽指向引擎保留键: %q", key)
		}
	}
	for _, root := range []string{LibRoot, SysRoot, DevRoot, VthreadRoot} {
		if key == root {
			return fmt.Errorf("写槽指向域根 %s: %q", root, key)
		}
	}
	for _, root := range []string{LibRoot, SysRoot, DevRoot} {
		if strings.HasPrefix(key, root+PathSegSep) {
			return fmt.Errorf("写槽指向受保护域 %s: %q", root, key)
		}
	}
	if strings.HasPrefix(key, VthreadRoot+PathSegSep) && !strings.HasPrefix(key, VThread(vtid)+PathSegSep) {
		if key == VThread(vtid) {
			// 自己的 vthread 根本身是帧根（目录），当叶子写会毁掉帧
			return fmt.Errorf("写槽指向本 vthread 的帧根: %q", key)
		}
		return fmt.Errorf("写槽越出本 vthread（vtid=%s）: %q", vtid, key)
	}
	return nil
}

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
