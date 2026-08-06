package keytree

import (
	"fmt"
	"strings"
)

// CheckWriteKey 校验一条写指令的最终落点。原生写与委托写共用同一套规则。
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
// 它不保证：不守协议的执行器被挡住。委托任务描述里带着 vtid 与 pc，执行器完全
// 可以自己推出任何键直接写（它按协议本来就在写 /sys/task/<id>.status）。封死那
// 一侧要靠 kvspace 的作用域凭证或居中 broker，不是这里。
//
// # 为什么只管写不管读
//
// 读侧刻意保持开放。委托出去的 agent 能读到指令集（/sys/rwir/）、源码（/lib/）、
// 活的调用栈、历史 vthread —— 这是 kvlang 相对「LLM SDK 绑定」的全部差异所在，
// 也是自分析、自进化那一步的前提。限制读会直接砍掉项目的核心论点。代价是程序
// 可以把任意键的值送给执行器；在「程序半可信、执行器按部署可信」这个模型下这是
// 可接受的，换模型时必须重新评估。
//
// # 规则
//
// 放行：调用方自己 vthread 子树内的槽位、用户全局键（不在四个域根下的路径）。
// 拒绝：
//   - 非规范路径（空段 / . / ..）。**不能指望后端替我们挡**：art 的
//     validateAbsolutePath 会拒，而 redis 后端一处路径校验都没有，只在出现 //
//     时 panic。同一段程序在两个后端下行为不同，必须在这里统一。
//     代价落在 redis 那侧：`{a=1} -> /g1/`、`set("/g2/","",1)` 这类在 art 下
//     本来就写不进去（只是失败被静默丢弃），在 redis 下**本来是能成功的**，
//     现在统一变成拒绝。这是有意的跨后端对齐，不是纯粹的收紧。
//   - 任一路径段以 ‥ 打头的引擎保留键 —— 写 ‥returnpc 可劫持控制流，写非 PC
//     值让下一轮取指裸 panic；写 ‥ro 可关掉只读参防线。
//   - /lib —— 改写运行中的代码（每次取指都重新从 KV 解码，注入立即生效）
//   - /sys —— 篡改后端注册表、伪造委托任务状态
//   - /dev —— device 层把值当文件路径 os.OpenFile(O_APPEND|O_CREATE) 或当
//     WebSocket URL 直连，等于任意文件写与 SSRF
//   - 四个域根本身，以及别的 vthread 子树
//   - 自己 vthread 的根：它是帧根（目录），当叶子写会毁掉帧结构
//
// 注意前缀比较一律带上分隔符：`set("/lib","pwned","X")` 产出的是
// Member("/lib","pwned") = "/lib.pwned"（点号不是斜杠），那是**根级**用户全局键、
// 不在代码区，放行 —— 别把它误判成绕过。真正危险的形态是 set("/lib/mylib",…)。
//
// /vthread 的对应形态**不对称，且是有意的**：`set("/vthread/999","s","X")` 产出
// "/vthread/999.s"，它虽然同样是点号兄弟键、进不了 vthread 999 的子树，但它
// **落在 /vthread 目录里面**，而 "/lib.pwned" 落在根目录。规则因此是
// 「/vthread 整个域归引擎，程序只能写自己 vthread 的子树」——比"只禁别的 vthread
// 子树"严，好处是简单可审计。
//
// 代价说清楚：连**自己** vtid 的点号兄弟键也一并拒了。
// `set("/vthread/1/","k",v)` → /vthread/1/.k（在子树内，放行）；
// `set("/vthread/1", "k",v)` → /vthread/1.k（在 /vthread 目录内，拒绝）——
// 同一个意图，差一个尾斜杠结果相反。改动前两种都能写。仓库里零处 .kv 这么用，
// 但它确实是一处可用能力的收窄，不是"只封了垃圾键"。
// 若要放开，把下面的判定从 HasPrefix(key, own+"/") 放宽成"以 own 开头且 != own"即可。
//
// # 已知边界：本函数的正确性依赖「/vthread 子树下不存在 link」这条不变量
//
// 校验的是**未解链**的字面路径，而 kvspace 的 Set 会跟随 link。实测：
// Link("/lib/secret/", "/probe/viaLink/") 之后写 /probe/viaLink/orig，
// /lib/secret/orig 真的被改了，还能在目标下建新键。所以只要 /vthread 子树里
// 存在一个指向受保护域的 link，本函数就会被绕过 —— 它看到的是安全的字面路径。
//
// 这条不变量当前成立，依据是三条互相独立的事实（都已实测/全仓核对）：
//  1. kvlang 全仓零处调用 kv.Link —— 程序拿不到这个原语。
//  2. kvlang 全仓零处构造 LinkIndex 值 —— 也就没法用 Set 把一个 link 写出来。
//     这一条要紧：art 后端的 Set 会校验目录值的 kind，而 **redis 后端的那个
//     kind 白名单只列了 Index 与 ExtIndex，漏了 LinkIndex**；换句话说防线不在
//     后端那边，而在"程序造不出这种值"。
//  3. extindex **不会**被写穿：kvspace 对 extindex 只读路径的写明确报错
//     （禁止对 extindex 只读路径执行写操作）。所以 HandleCall 给每个帧建的
//     ‥stack → /lib/<func>/ 覆盖不是穿透向量 —— 这是最容易误判为漏洞的一处。
//
// 残余风险：别的进程（kvspace CLI、外部执行器）在 /vthread 下埋 link。
// 那属于「执行器不可信」那一侧，见上面的威胁模型。
// rwir/builtin 的架构测试钉住了 (1)(2)，防止将来有人加一个建 link 的内建。
//
// # 与后续规划的一处已知冲突
//
// article/self-evolving-robot.md 设想自进化闭环里程序要写
// /sys/heap/robot/weights_v{n} 与 /sys/heap/robot/replay。本规则把 /sys 整域
// 封死，那条路会被挡住。该能力目前标注为未支持，所以现在不冲突；等 heap 落地时
// 需要在这里为 /sys/heap 开一个口子（并想清楚它与"程序半可信"的关系）。
func CheckWriteKey(vtid, key string) error {
	if key == "" || key[0] != PathSegSep[0] {
		return fmt.Errorf("PermissionError: 写槽不是绝对路径: %q; help: 写槽必须解析成 / 开头的规范路径", key)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(key, PathSegSep), PathSegSep) {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return fmt.Errorf("PermissionError: 写槽不是规范路径: %q; help: 路径里不得有空段、. 或 ..", key)
		case strings.HasPrefix(seg, RuntimeMemberSep):
			return fmt.Errorf("PermissionError: 写槽指向引擎保留键: %q; help: 以 %s 打头的键归 VM 所有，程序不可写",
				key, RuntimeMemberSep)
		}
	}
	for _, root := range []string{LibRoot, SysRoot, DevRoot, VthreadRoot} {
		if key == root {
			return fmt.Errorf("PermissionError: 写槽指向域根 %s: %q; help: 域根是目录，不能当叶子写", root, key)
		}
	}
	for _, root := range []string{LibRoot, SysRoot, DevRoot} {
		if strings.HasPrefix(key, root+PathSegSep) {
			return fmt.Errorf("PermissionError: 写槽指向受保护域 %s: %q; help: %s 归 VM 所有；请写自己 vthread 的槽位或用户全局键",
				root, key, root)
		}
	}
	if strings.HasPrefix(key, VthreadRoot+PathSegSep) {
		own := VThread(vtid)
		if key == own {
			return fmt.Errorf("PermissionError: 写槽指向本 vthread 的帧根: %q; help: 帧根是目录，当叶子写会毁掉调用帧", key)
		}
		if !strings.HasPrefix(key, own+PathSegSep) {
			// 措辞要分清两种情形，否则会误导：
			//   /vthread/999/x    真的伸进了别人的帧树
			//   /vthread/1.k      vtid 就是自己的，只是这个键挂在 /vthread 目录下、
			//                     不在自己子树里（Member 拼的是点号）
			// 后者说"越出本 vthread"是错的 —— 程序哪儿也没越，是 /vthread 整个域
			// 归引擎所有。
			return fmt.Errorf("PermissionError: 写槽不在本 vthread 的子树内（vtid=%s）: %q; "+
				"help: /vthread 整域归引擎，程序只能写 %s/ 之下的键", vtid, key, own)
		}
	}
	return nil
}
