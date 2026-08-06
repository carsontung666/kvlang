package parser

import (
	"strings"
	"testing"
)

// hasErrorContaining 报告诊断里是否有 error 级、且消息含 sub 的一条。
func hasErrorContaining(diags []Diagnostic, sub string) bool {
	for _, d := range diags {
		if !d.Warn && !d.Info && strings.Contains(d.Message, sub) {
			return true
		}
	}
	return false
}

// TestParseFuncSigDottedName 锁住 ParseFuncSig 对带点名字的处理。
// 此前遇到 Dot 就提前结束名字，随后 peek 不是 LParen，参数表与返回值被整段
// 跳过 —— 静默产出一个没有参数的签名。
func TestParseFuncSigDottedName(t *testing.T) {
	for _, tc := range []struct {
		sig      string
		wantName string
		wantRead int
		wantWrit int
	}{
		{"rwfunc add(a:int64, b:int64) -> (c:int64)", "add", 2, 1},
		{"rwir fake.echo(a:string) -> (b:string)", "fake.echo", 1, 1},
		{"rwir llm.chat(prompt:string, model:string) -> (reply:string)", "llm.chat", 2, 1},
		{"rwir a.b.c(x:string) -> (y:string)", "a.b.c", 1, 1},
	} {
		got := ParseFuncSig(tc.sig)
		if got.Name != tc.wantName {
			t.Errorf("ParseFuncSig(%q).Name = %q，期望 %q", tc.sig, got.Name, tc.wantName)
		}
		if int(got.NumReads()) != tc.wantRead || int(got.NumWrites()) != tc.wantWrit {
			t.Errorf("ParseFuncSig(%q)：读参 %d 写参 %d，期望 %d/%d",
				tc.sig, got.NumReads(), got.NumWrites(), tc.wantRead, tc.wantWrit)
		}
	}
}

// TestRwirDeclInLibKeepsInitEmpty 是那条「静默变空操作」的解析层断言：
// lib 块里的 rwir 声明不得往 InitBody 里塞任何东西。
func TestRwirDeclInLibKeepsInitEmpty(t *testing.T) {
	df, diags, err := ParseCode(strings.NewReader(`
lib llm {
	rwir chat(prompt:string) -> (reply:string)
}

rwfunc main() -> () {
	40 + 2 -> answer
}

main()
`))
	if err != nil {
		t.Fatalf("ParseCode: %v", err)
	}
	if HasErrors(diags) {
		t.Fatalf("不该有 error 级诊断：%v", diags)
	}
	if len(df.InitBody) != 0 {
		t.Fatalf("lib 内的 rwir 声明往 InitBody 塞了 %d 条幻影语句（会让 /lib/init/[0,0] "+
			"变成空 opcode，vthread 立刻 SetDone）", len(df.InitBody))
	}
	if len(df.RwirDecls) != 1 || df.RwirDecls[0].Sig.Name != "chat" || df.RwirDecls[0].Pkg != "llm" {
		t.Fatalf("声明未正确归属：%+v", df.RwirDecls)
	}
	if len(df.TopLevelCalls) != 1 {
		t.Fatalf("顶层调用 main() 丢失：实得 %d 条", len(df.TopLevelCalls))
	}
}

// TestNamelessDeclIsSyntaxError 锁住无名声明必须报 error。
// 放过去的话 WriteRwir/WriteFunc 会 DelTree(LibFunc("", "")) = DelTree("/lib")，
// 静默抹掉整个函数库。
func TestNamelessDeclIsSyntaxError(t *testing.T) {
	for _, src := range []string{
		"rwir\n",
		"rwir 9op(a:string) -> (b:string)\n",
		"rwir .x(a:string) -> (b:string)\n",
		"rwir (a:string) -> (b:string)\n",
		"rwfunc (a:string) -> (b:string) { a -> b }\n",
		"rwfunc { }\n",
	} {
		_, diags, err := ParseCode(strings.NewReader(src))
		if err != nil {
			t.Fatalf("ParseCode(%q): %v", src, err)
		}
		if !hasErrorContaining(diags, "缺少名字") {
			t.Errorf("源码 %q 应报「缺少名字」，实得诊断 %v", src, diags)
		}
	}
}

// TestNamedDeclHasNoNameError 反向确认上一条没有误伤正常声明。
func TestNamedDeclHasNoNameError(t *testing.T) {
	for _, src := range []string{
		"rwir fake.echo(a:string) -> (b:string)\n",
		"rwir bare(a:string) -> (b:string)\n",
		"lib llm {\n\trwir chat(p:string) -> (r:string)\n}\n",
		"rwfunc f(a:string) -> (b:string) { a -> b }\n",
	} {
		_, diags, err := ParseCode(strings.NewReader(src))
		if err != nil {
			t.Fatalf("ParseCode(%q): %v", src, err)
		}
		if hasErrorContaining(diags, "缺少名字") {
			t.Errorf("源码 %q 不该报「缺少名字」，实得诊断 %v", src, diags)
		}
	}
}

// TestDanglingDotIsSyntaxError 锁住"点后面必须跟标识符"。
// 少了这道检查，`rwir fake.(a:string) -> (b:string)` 会一路解析通过、无诊断，
// 写出名字为 "fake." 的畸形键 /lib/fake. —— 既不可能被调用点命中，也不可能
// 被路由（splitOp 会切出空算子名）。实测未修前 kvlang layout 退出码为 0。
func TestDanglingDotIsSyntaxError(t *testing.T) {
	for _, src := range []string{
		"rwir fake.(a:string) -> (b:string)\n",
		"rwir a.b.(x:string) -> (y:string)\n",
		"lib llm {\n\trwir chat.(p:string) -> (r:string)\n}\n",
		"rwfunc f.(a:string) -> (b:string) { a -> b }\n",
	} {
		_, diags, err := ParseCode(strings.NewReader(src))
		if err != nil {
			t.Fatalf("ParseCode(%q): %v", src, err)
		}
		if !hasErrorContaining(diags, "'.' 后面缺少标识符") {
			t.Errorf("源码 %q 应报点后缺标识符，实得诊断 %v", src, diags)
		}
	}
}

// TestRwirDeclDottedName 确认声明侧也支持多级点号命名空间。
func TestRwirDeclDottedName(t *testing.T) {
	df, diags, err := ParseCode(strings.NewReader("rwir a.b.c(x:string) -> (y:string)\n"))
	if err != nil {
		t.Fatalf("ParseCode: %v", err)
	}
	if HasErrors(diags) {
		t.Fatalf("不该有 error 级诊断：%v", diags)
	}
	if len(df.RwirDecls) != 1 {
		t.Fatalf("期望 1 条声明，实得 %d", len(df.RwirDecls))
	}
	d := df.RwirDecls[0]
	if d.Sig.Name != "a.b.c" || d.Sig.NumReads() != 1 || d.Sig.NumWrites() != 1 {
		t.Fatalf("声明解析错误：name=%q 读参=%d 写参=%d", d.Sig.Name, d.Sig.NumReads(), d.Sig.NumWrites())
	}
}
