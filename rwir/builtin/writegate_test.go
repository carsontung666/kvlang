package builtin

// 架构测试：钉住「程序控制的写入点集合」与「程序造不出 link」两条不变量。
//
// 它不断言运行时行为——行为测试与变异测试负责那部分。它挡的是**行为测试挡不住
// 的那类回归**：有人新加一个内建、顺手写一条 f.KV.Set(...)，于是多出第 N 扇没
// 过落点校验的门。这一轮之前的实现连踩三次都是这个形态：每次都"以为守住了"，
// 因为已有的行为测试全绿，而新门根本没有对应的用例。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 允许直接调用 kv 变更接口的位置，**按文件记预期条数**。
//
// 只按文件名豁免是不够的：helper.go 恰恰是下一个写辅助函数最可能落地的地方，
// 整file 豁免会让绊线在最可能失效的点上失明。记条数之后，往 helper.go 里新加
// 一条 Set 也会让本用例变红，必须显式更新这里并说明理由。
var allowedRawMutations = map[string]struct {
	count  int
	reason string
}{
	// kvSet（统一写入口，writeResult 与各内建都经它）+ ExecuteCopy 的批量写。
	// 两处的键都来自已校验的 resolveWriteSlot。
	"helper.go": {2, "kvSet 统一写入口 + ExecuteCopy 的批量写，键均来自已校验的解析函数"},
	// VM 内部行为：启动时把内建算子签名发布到 /sys/rwir/*，与程序输入无关。
	// 按包一刀切会把它也拦掉，VM 直接起不来 —— 所以校验挂在写槽解析层而不是 Set 层。
	"ops.go": {1, "WriteSysRwir —— VM 启动时发布内建签名到 /sys/rwir，非程序控制"},
	// debugger 只碰自己 vthread 的 ‥debugger 键，路径固定、非程序控制。
	"debugger.go": {2, "debuggerOp 的 Del/Notify，键是固定的 ‥debugger 布局"},
}

// 会改变 kvspace 状态的接口。只盯 .Set( 是不够的 —— 将来有人加一个
// kv.Del(<程序控制的键>) 或 kv.Link(...) 的内建，同样是一扇没上锁的门。
var mutatingOps = []string{"Set", "Del", "DelTree", "Notify", "Link", "UnLink", "ExtIndex", "Mkindex", "Clear"}

// stripCodeComments 去掉行尾注释与整行注释，避免注释里出现 kv.Set( 造成误报。
func stripCodeComments(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

func goSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		out[name] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("一个源文件都没读到，测试本身失效了")
	}
	return out
}

// TestNoUncheckedWriteDoor 确认 rwir/builtin 下不存在白名单之外的 kv 变更调用点，
// 且白名单文件里的条数没有悄悄增长。
func TestNoUncheckedWriteDoor(t *testing.T) {
	// 匹配 xxx.<变更接口>( 但排除 vthread.Set(（那是引擎 PC 状态，不是 KV 写）
	call := regexp.MustCompile(`(\w+(?:\.\w+)*)\.(` + strings.Join(mutatingOps, "|") + `)\(`)
	found := map[string]int{}
	for name, src := range goSources(t) {
		for i, line := range strings.Split(src, "\n") {
			for _, m := range call.FindAllStringSubmatch(stripCodeComments(line), -1) {
				if m[1] == "vthread" {
					continue
				}
				found[name]++
				if _, ok := allowedRawMutations[name]; ok {
					continue
				}
				t.Errorf("%s:%d 出现未过落点校验的写入点 %s.%s(（第 N 扇门）\n"+
					"  → 写入前必须经 resolveWriteSlot / writeSlotKey / checkMemberKey 取得 key；\n"+
					"  → 若确属 VM 内部写，把文件加进 allowedRawMutations 并写明理由。\n"+
					"  行内容: %s", name, i+1, m[1], m[2], strings.TrimSpace(line))
			}
		}
	}
	// 条数守卫：白名单文件里新增一条也要变红
	for name, want := range allowedRawMutations {
		if got := found[name]; got != want.count {
			t.Errorf("%s 的直接写调用条数从 %d 变成 %d（白名单理由：%s）\n"+
				"  → 新增的那条是否过了落点校验？确认后更新 allowedRawMutations 的 count。",
				name, want.count, got, want.reason)
		}
	}
}

// TestProgramCannotCreateLink 钉住落点校验所依赖的不变量：程序造不出 link。
//
// 校验的是未解链的字面路径，而 kvspace 的 Set 会跟随 link。只要程序能在自己
// 的 /vthread 子树里造一个指向 /lib 的 link，整道校验就被绕过。
// 依赖的两件事：kvlang 从不调 kv.Link，也从不构造 LinkIndex 值
// （后者尤其要紧——redis 后端的目录值 kind 白名单漏了 LinkIndex）。
func TestProgramCannotCreateLink(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			if base == ".git" || base == "deepx-design" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx] // 跳过注释（本包与 keytree 的文档里就提到这些名字）
			}
			if strings.Contains(code, ".Link(") || strings.Contains(code, "NewLinkIndex") {
				offenders = append(offenders, p+":"+strconv.Itoa(i+1)+" "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("出现了创建 link 的代码，落点校验的不变量被打破：\n  %s\n"+
			"  → link 会让 Set 写穿到目标路径，而校验看到的是未解链的字面路径。\n"+
			"  → 若确实需要 link，必须同时让 CheckWriteKey 解链后再判定。",
			strings.Join(offenders, "\n  "))
	}
}
