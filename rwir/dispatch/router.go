package dispatch

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/array2d/kvspace-go"
	"kvlang/keytree"
	"kvlang/logx"
)

// Select 为 opcode 挑一个可用后端，返回后端名。
// 调用方用 keytree.SysRwirBackendCmd(backend) 构造命令队列。
//
// 注册布局见 keytree.SysRwirBackendRoot 的注释。一级路由 —— 后端名即实例名，
// 因为 rwir 后端比单 op 后端"重"，通常一类只有一个（一个 LLM worker pool、
// 一个 agent pool）。要横向扩容就注册成 llm-0 / llm-1 两个后端名。
//
// 候选来自注册表本身，不再从 opcode 的命名空间反推后端名：命名空间是源码里的
// 库名，后端名是部署时起的，二者没有必须相等的理由。
func Select(ctx context.Context, kv kvspace.KVSpace, opcode string) (backend string, err error) {
	// 两个名单分开报，诊断才指得准：「没人支持这个算子」是拼错算子名或忘了装后端，
	// 「支持但都不在岗」是执行器挂了 —— 处置完全不同。
	registered, onDuty := backendsFor(kv, opcode)
	if len(registered) == 0 {
		return "", fmt.Errorf("no backend supports opcode=%s", opcode)
	}
	if len(onDuty) == 0 {
		// 带上各自的 status 实值：最常见的病因是词表写错（旧协议的 "running"、
		// 或 shell 拼出来带尾随换行），只报"没有在岗后端"指不到那儿。
		return "", fmt.Errorf("no on-duty backend for opcode=%s; 后端 status 需为 ready|busy，实得 %v",
			opcode, statusesOf(kv, registered))
	}

	// 以首个候选起手而不是从 math.MaxFloat64 比下来：后者在「全部候选的 load 都
	// 大到不小于哨兵」时会返回空后端名，任务随即被推到一条畸形队列路径上，而那条
	// 分支没有任何输入能触发、也就无法被测试覆盖。这样写从结构上就不可能返回空。
	backend = onDuty[0]
	bestLoad := parseLoad(kv, backend)
	for _, b := range onDuty[1:] {
		// 严格小于：等负载时先注册者胜，钉在 TestSelectEqualLoadIsStable
		if load := parseLoad(kv, b); load < bestLoad {
			bestLoad, backend = load, b
		}
	}
	return backend, nil
}

// parseLoad 读后端自报负载。缺省 0 —— 后端可以只报 status 就开工，不强制上报。
//
// **不可比较的值一律视为满载 1.0**，不能沿用缺省的 0：注册表是外部进程写的，
// 而 0 是最低负载，把坏值当 0 会让一个写坏了 load 的后端赢过所有正常后端、吸走
// 全部流量。取 1.0 而非直接排除，是为了它在"只有它一个"时仍可用。
//
// 光判 err != nil 不够 —— 这两类都能通过 ParseFloat 却毁掉路由：
//
//	"NaN"          与任何数比较都是 false，于是 `load >= bestLoad` 恒不成立，
//	               每个后端都覆盖前一个，「负载最低」无声退化成「最后注册的赢」。
//	"-5" / "-Inf"  负值恒小于一切，该后端永远独占路由。
//
// 超出 1 的有限值**故意放行**：文档说 load 是 [0,1]，但后端误报绝对计数
// （5 / 3 / 9 这种）时，相对大小仍然是有意义的路由信息；一律夹到 1.0 反而
// 把这份信息抹平成"先注册的赢"。只有不可比较的值才需要兜底。
func parseLoad(kv kvspace.KVSpace, backend string) float64 {
	v := kvspace.GetOne(kv, keytree.SysRwirBackendLoad(backend))
	if kvspace.IsNone(v) {
		return 0
	}
	raw := strings.TrimSpace(v.String())
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		logx.Debug("Select: backend %s load=%q 不是可比较的非负数，按满载处理", backend, raw)
		return 1
	}
	return f
}

// listChildren 列出目录的直接子项名，并剥掉目录项的尾斜杠。
//
// kv.List 对目录子项返回带尾斜杠的名字（叶子项不带）：/sys/rwir-backend/ 下的
// 后端全是目录，返回的是 "fake/" 而非 "fake"。不剥离就会拼出
// /sys/rwir-backend/fake//op/x，
// 而 kvspace 对非规范路径是 panic 而非报错。
func listChildren(kv kvspace.KVSpace, dir string) []string {
	names := kv.List(dir, false)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, strings.TrimSuffix(name, keytree.PathSegSep))
	}
	return out
}

// IsDelegatedOp 判断 opcode 是否交给外部后端执行 —— **判据是后端注册表**：
// 有任一**在岗**后端在 /sys/rwir-backend/<b>/op/<opcode> 声明了这个算子即委托。
//
// 判据在运行时、由后端自报，不在编译期由源码决定。这是刻意的：起一个 GPU 后端，
// tensor.matmul 当场变成可委托，源码一个字不用改；后端全部下线则回落到本地路径。
// 「哪些自己执行、哪些交出去」因此是一张可变的表，不是一次性绑定。
//
// **必须与 Select 用同一个 offering 判定**，否则会出现「判定为可委托、但选不出
// 后端」的空档：那时 Delegate 直接 SetError 硬失败，既不回落本地、也没有任何
// 后端能接手。文档 01 明确要求注销时只写 status=offline、不删 op 子键（记录留着
// 排障），所以「op 子键存在」不等于「有人在岗」—— 只看存在性会让一个永久死掉的
// 后端永久霸占这个 opcode。
//
// 与本地定义的关系：本函数排在 execute.go 调度链的 IsNativeOp 之后，所以内建
// 算子永远赢过后端注册；但它排在 default（rewrite as call）之前，所以在岗后端
// 会盖过 /lib 里的同名 rwfunc。要让某个算子固定走本地，别让后端注册它。
func IsDelegatedOp(kv kvspace.KVSpace, opcode string) bool {
	_, onDuty := backendsFor(kv, opcode)
	return len(onDuty) > 0
}

// backendsFor 扫注册表，返回声明了该 opcode 的全部后端与其中在岗的那些，
// 均保持 kv.List 的顺序。IsDelegatedOp 与 Select 共用它，两者不可能给出
// 矛盾的结论；registered 只用于诊断，不参与路由。
func backendsFor(kv kvspace.KVSpace, opcode string) (registered, onDuty []string) {
	// opcode 直接当路径段用，而它是**程序可控**的：`/lib/math.sum(3,4) -> s` 这种
	// 绝对路径调用形态（仓库 tutorial/06-lib/cross/inline.kv 就是）会让 opcode 带上
	// 斜杠，拼出 /sys/rwir-backend/<b>/op//lib/ —— art 后端对非规范路径是 panic，
	// 整个 VM 当场带栈死掉。这种名字也不可能有后端注册得上，直接判否。
	if !keytree.ValidSegment(opcode) {
		return nil, nil
	}
	for _, b := range listChildren(kv, keytree.SysRwirBackendRoot+keytree.PathSegSep) {
		if kvspace.IsNone(kvspace.GetOne(kv, keytree.SysRwirBackendOp(b, opcode))) {
			continue
		}
		registered = append(registered, b)
		if isOnDuty(kv, b) {
			onDuty = append(onDuty, b)
		}
	}
	return registered, onDuty
}

// isOnDuty 判断后端是否在岗。词表取自文档 01：ready | busy | offline。
//
//	ready    在岗且空闲
//	busy     在岗但满载 —— **仍然算在岗**。把 busy 当不可用会让一个短暂繁忙的
//	         后端触发硬失败，而它自报的 load 本来就足以让 Select 避开它。
//	offline  不在岗（注销或心跳超时）
//	缺失      不在岗 —— 注册是多次写入，op 子键先落、status 后落是常态，
//	         这个窗口里必须当它没准备好，否则任务会投给还没就绪的进程。
// statusesOf 收集后端的 status 实值，只用于诊断。
func statusesOf(kv kvspace.KVSpace, backends []string) map[string]string {
	out := make(map[string]string, len(backends))
	for _, b := range backends {
		out[b] = kvspace.GetOne(kv, keytree.SysRwirBackendStatus(b)).String()
	}
	return out
}

// TrimSpace 与 parseLoad 保持一致：注册表由外部进程写，`echo ready | kvspace set`
// 这类 shell 后端会带上尾随换行，裸比较的话后端"注册成功了但 VM 永远看不见"，
// 而报出的错是 no on-duty backend，一点都指不到病因。
func isOnDuty(kv kvspace.KVSpace, backend string) bool {
	switch strings.TrimSpace(kvspace.GetOne(kv, keytree.SysRwirBackendStatus(backend)).String()) {
	case "ready", "busy":
		return true
	}
	return false
}

// delegatedSig 读 opcode 的 /lib 签名键（源码里的 `rwir name(...) -> (...)`）。
// ok=false 表示没有这么一条声明 —— 这**不**代表不该委托：判据是注册表，声明
// 只是可选的参数个数契约。有声明就校验，没有就放行。
func delegatedSig(kv kvspace.KVSpace, opcode string) (sig kvspace.XValue, ok bool) {
	v := kvspace.GetOne(kv, keytree.FuncKey(opcode))
	if kvspace.IsNone(v) || v.Kind() != kvspace.KindRwir {
		return nil, false
	}
	return v, true
}

// opcode 不再被拆分 —— 注册表里存的就是完整 opcode（见 keytree.SysRwirBackendOp）。
// 原先按最后一个点拆出命名空间当后端名的 splitOp 已随之删除。
