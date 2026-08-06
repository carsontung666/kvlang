package layout_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/array2d/kvspace-go"
	_ "github.com/array2d/kvspace-go/art"

	"kvlang/ast"
	"kvlang/keytree"
	"kvlang/layout"
	"kvlang/logx"
	"kvlang/parser"
)

// TestWriteNamelessDoesNotWipeLib 锁住写入侧的第二道防线。
//
// WriteRwir/WriteFunc 开头都会 DelTree(LibFunc(pkg, name))；空名 + 空包名
// = DelTree("/lib")，一次调用抹掉整个函数库。parser 已对无名声明报错，但
// 「代价 = 整个 /lib」的东西不能只防一层：任何绕过 parser 的调用方
// （lower 后手工构造 ast.Func、agent 直接调 layout）都会踩到。
func TestWriteNamelessDoesNotWipeLib(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(kv kvspace.KVSpace)
	}{
		{"WriteRwir 空名", func(kv kvspace.KVSpace) {
			layout.WriteRwir(kv, "", &ast.RwirDecl{})
		}},
		{"WriteFunc 空名", func(kv kvspace.KVSpace) {
			layout.WriteFunc(kv, "", &ast.Func{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := kvspace.Conn("art://")
			kv.DelTree(keytree.LibRoot)
			kvspace.MkIndexRecursive(kv, keytree.LibRoot+keytree.PathSegSep)
			// 放一个"已有的函数库"进去（值类型无所谓，只关心它还在不在）
			if err := kv.Set([]kvspace.KVPair{
				{Key: keytree.LibFunc("", "survivor"), Val: kvspace.NewChar("existing")},
			}); err != nil {
				t.Fatalf("布置前置数据失败: %v", err)
			}

			tc.write(kv)

			if v := kvspace.GetOne(kv, keytree.LibFunc("", "survivor")); kvspace.IsNone(v) {
				t.Fatal("无名写入把整个 /lib 抹掉了（DelTree 退化成 DelTree(\"/lib\")）")
			}
			kv.DelTree(keytree.LibRoot)
			kvspace.MkIndexRecursive(kv, keytree.LibRoot+keytree.PathSegSep)
		})
	}
}

// freshLib 清出一个干净的 /lib（art:// 是进程内单例，用例之间会串）。
func freshLib(t *testing.T) kvspace.KVSpace {
	t.Helper()
	kv := kvspace.Conn("art://")
	kv.DelTree(keytree.LibRoot)
	kvspace.MkIndexRecursive(kv, keytree.LibRoot+keytree.PathSegSep)
	t.Cleanup(func() {
		kv.DelTree(keytree.LibRoot)
		kvspace.MkIndexRecursive(kv, keytree.LibRoot+keytree.PathSegSep)
	})
	return kv
}

// loadDecls 走真实的 parser 而不是手搭 ast.File —— 手搭能造出 parser 根本
// 产不出的状态（比如 df.Package 非空：parseFile 从不设置它，parseLibBody 设了
// 又无条件还原），那样测出来的只是一条死分支。
func loadDecls(t *testing.T, kv kvspace.KVSpace, src string) *ast.File {
	t.Helper()
	df, diags, err := parser.ParseCode(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parser.HasErrors(diags) {
		t.Fatalf("parse diagnostics: %v", diags)
	}
	layout.WriteDecls(kv, df)
	return df
}

// TestWriteDeclsPlacesDeclsByLib 确认声明按 lib 归属落键：lib 块内的带包名，
// 顶层的落在 /lib 根下。
func TestWriteDeclsPlacesDeclsByLib(t *testing.T) {
	kv := freshLib(t)
	loadDecls(t, kv, `
rwir toplevel(a:string) -> (b:string)

lib llm {
	rwir chat(prompt:string) -> (reply:string)
}

lib outer {
	lib inner {
		rwir deep(a:string) -> (b:string)
	}
}
`)
	for _, key := range []string{"/lib/toplevel", "/lib/llm.chat", "/lib/outer/inner.deep"} {
		v := kvspace.GetOne(kv, key)
		if kvspace.IsNone(v) {
			t.Fatalf("%s 未写入", key)
		}
		if v.Kind() != kvspace.KindRwir {
			t.Fatalf("%s 的 kind = %q，期望 %q", key, v.Kind(), kvspace.KindRwir)
		}
	}
}

// TestWriteRwirRoundTrip 确认写进 /lib 的不只是"一个 kind=rwir 的值"，
// 而是 arity 与签名串都正确 —— 只查 !IsNone + Kind() 的话，把 arity 写死成 0
// 或把签名串写空都察觉不到，而派发期的读写参个数校验正是靠这两个数。
func TestWriteRwirRoundTrip(t *testing.T) {
	kv := freshLib(t)
	loadDecls(t, kv, "rwir llm.chat(prompt:string, model:string) -> (reply:string)\n")

	v := kvspace.GetOne(kv, "/lib/llm.chat")
	r, ok := v.(kvspace.Rwir)
	if !ok {
		t.Fatalf("/lib/llm.chat 不是 Rwir，实得 %T", v)
	}
	if r.NumReads() != 2 || r.NumWrites() != 1 {
		t.Errorf("存下来的 arity = 读 %d 写 %d，期望 2/1", r.NumReads(), r.NumWrites())
	}
	// 签名串必须能被解析回等价的签名
	sig := parser.ParseFuncSig(r.Sig())
	if sig.Name != "llm.chat" || sig.NumReads() != 2 || sig.NumWrites() != 1 {
		t.Errorf("签名串 %q 解析回来是 name=%q 读 %d 写 %d", r.Sig(), sig.Name, sig.NumReads(), sig.NumWrites())
	}
}

// TestWriteDeclsWarnsOnShadowedDecl 确认"声明写了却永远不会被派发"的两种情形
// 都会出声。静默的代价是用户以为接上了外部执行器、实际跑的是别的东西且 exit 0。
//
// 断言的是 logx 实际写出的告警文本，不是某个内部标志位。
func TestWriteDeclsWarnsOnShadowedDecl(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{
			"被同名 rwfunc 覆盖",
			"rwir dup(a:string) -> (b:string)\nrwfunc dup(a:string) -> (b:string) { a -> b }\n",
			"被同名 rwfunc 覆盖",
		},
		{
			"与内建算子同名",
			"rwir println(a:string) -> ()\n",
			"与内建算子同名",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := freshLib(t)
			var buf bytes.Buffer
			restore := logx.SetOutputForTest(&buf)
			defer restore()

			loadDecls(t, kv, tc.src)

			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("应告警 %q，实得日志 %q", tc.want, buf.String())
			}
		})
	}
}

// TestWriteDeclsQuietWhenNoShadowing 反向确认没有误报。
func TestWriteDeclsQuietWhenNoShadowing(t *testing.T) {
	kv := freshLib(t)
	var buf bytes.Buffer
	restore := logx.SetOutputForTest(&buf)
	defer restore()

	loadDecls(t, kv, `
rwir llm.chat(prompt:string) -> (reply:string)

lib svc {
	rwir print(a:string) -> ()
}

rwfunc other(a:string) -> (b:string) { a -> b }
`)
	if strings.Contains(buf.String(), "覆盖") || strings.Contains(buf.String(), "同名") {
		t.Fatalf("不该告警（svc.print 带命名空间，与内建 print 不冲突），实得 %q", buf.String())
	}
}

// TestWriteRwirClearsStaleKey 确认重复装载会清掉旧内容，而不是叠加。
// 声明是可以改的：改完重新 layout，旧的子项不该留下来。
func TestWriteRwirClearsStaleKey(t *testing.T) {
	kv := freshLib(t)
	loadDecls(t, kv, "rwir svc.op(a:string) -> (b:string)\n")
	// 在签名键下埋一个子项，模拟上一次装载留下的残骸
	kvspace.MkIndexRecursive(kv, "/lib/svc.op/")
	kv.Set([]kvspace.KVPair{{Key: "/lib/svc.op/stale", Val: kvspace.NewChar("x")}})

	loadDecls(t, kv, "rwir svc.op(a:string, b:string) -> (c:string)\n")

	if v := kvspace.GetOne(kv, "/lib/svc.op/stale"); !kvspace.IsNone(v) {
		t.Fatalf("重新装载后仍留着旧子项 /lib/svc.op/stale = %q", v.String())
	}
	r, ok := kvspace.GetOne(kv, "/lib/svc.op").(kvspace.Rwir)
	if !ok || r.NumReads() != 2 {
		t.Fatalf("签名未被新声明替换，实得 %#v", kvspace.GetOne(kv, "/lib/svc.op"))
	}
}
