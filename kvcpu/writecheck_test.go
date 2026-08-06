package kvcpu_test

// 写落点校验的端到端测试：源码 → 装载 → 执行 → 断言被拒且目标键为空。
//
// 为什么放在 kvcpu 而不是 rwir/builtin：要证明的是"程序真的写不进去"，那是整条
// 执行链的性质，只调 builtin 的某个函数证明不了。
//
// 为什么必须有这一层（而不是只留 tutorial/error_cases 的 .kv 用例）：变异测试
// 实测发现，把门 2（writeSlotKey）、门 3（checkMemberKey）、逐槽校验、
// 拒绝时写 ‥error/msg 这四处逐个改坏，`go test ./...` 全都照绿 —— 只有端到端
// 矩阵能杀掉。而 error_test.py 目前没接进 CI（它有两个既有的失败用例），
// 于是那四处等于没有自动化防线。这个文件补的就是这个缺口。

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/array2d/kvspace-go"
	_ "github.com/array2d/kvspace-go/art"

	"kvlang/ast"
	"kvlang/keytree"
	"kvlang/kvcpu"
	"kvlang/layout"
	"kvlang/lower"
	"kvlang/parser"
	"kvlang/rwir/builtin"
	"kvlang/vthread"
)

// art:// 是进程内全局单例，Conn 不给独立实例、DisConn 也不清空，
// 用例之间必须显式清根；vtid 还要单调递增，否则跨用例复用同一棵帧树。
var vtidBase = 5000

func newKV(t *testing.T) kvspace.KVSpace {
	t.Helper()
	kv := kvspace.Conn("art://")
	for _, root := range []string{keytree.LibRoot, keytree.VthreadRoot, keytree.SysOpRoot, keytree.SysTaskRoot} {
		kv.DelTree(root)
	}
	kvspace.MkIndexRecursive(kv, keytree.LibRoot+keytree.PathSegSep)
	kvspace.MkIndexRecursive(kv, keytree.VthreadRoot+keytree.PathSegSep)
	vtidBase += 100
	kv.Set([]kvspace.KVPair{{Key: keytree.VthreadSeq, Val: kvspace.NewChar(strconv.Itoa(vtidBase))}})
	return kv
}

// runSrc 照搬 cmd/kvlang 的装载流程：顶层调用包成 /lib/init 再 Bootstrap init。
// 直接 Bootstrap 一个普通 rwfunc 会绕过控制流，scope 帧只布局不执行。
func runSrc(t *testing.T, kv kvspace.KVSpace, src string) (string, error) {
	t.Helper()
	df, diags, err := parser.ParseCode(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parser.HasErrors(diags) {
		t.Fatalf("parse diagnostics: %v", diags)
	}
	layout.WriteDecls(kv, df)
	for i := range df.Funcs {
		fpkg := df.Funcs[i].Pkg
		if fpkg == "" {
			fpkg = df.Package
		}
		layout.WriteFunc(kv, fpkg, lower.Func(&df.Funcs[i]))
	}
	body := df.InitBody
	for _, c := range df.TopLevelCalls {
		body = append(body, c)
	}
	if len(body) > 0 {
		initFn := ast.Func{Sig: ast.FuncSig{Name: "init"}, Body: body}
		layout.WriteFunc(kv, "", lower.Func(&initFn))
	}
	ctx := context.Background()
	vtid := vthread.AllocVtid(kv)
	kv.DelTree(keytree.VThread(vtid))
	kvspace.MkIndexRecursive(kv, keytree.VThread(vtid)+keytree.PathSegSep)
	builtin.WriteSysRwir(kv)
	pc := layout.Bootstrap(ctx, kv, vtid, "init", nil)
	if pc == "" {
		t.Fatal("Bootstrap init failed")
	}
	vthread.Set(ctx, kv, vtid, pc, "init")
	return vtid, kvcpu.New(kv, "test").Execute(pc)
}

// TestWriteToProtectedKeyIsDenied 穷举各条**独立的**写实现，每行对应一个不同的
// 写入点。守住其中一条等于没守 —— 换个算子就绕过去了，这一轮之前的实现连踩三次
// 都是这个形态。
func TestWriteToProtectedKeyIsDenied(t *testing.T) {
	// noRead: 键本身非规范，读不得 —— art 后端的 Get/List 对非规范路径**直接
	// panic**（这正是校验函数必须是纯字符串函数、不能碰 kvspace 的原因）。
	// 这类行只靠"错误信息里出现了这个键"来确认落点，不做 GetOne。
	for _, tc := range []struct {
		name, body, key, want string
		noRead                bool
	}{
		// 门 1：resolveWriteSlot（查 ‥wparam 重定向）
		{"算术 writeResult", `1 + 1 -> /lib/pwned`, "/lib/pwned", "受保护域", false},
		{"copy ExecuteCopy", `"X" -> /lib/pwned`, "/lib/pwned", "受保护域", false},
		// 逐槽：第 2 个写槽必须同样被拦（校验放循环外就漏这里）
		{"copy 第2个写槽", `"SRC" -> ok, /lib/slot2`, "/lib/slot2", "受保护域", false},
		// 门 2：writeSlotKey（不查 ‥wparam 的第二条解析路径）
		{"dict 字面量", `{ a=1 } -> /lib/pwned`, "/lib/pwned", "受保护域", false},
		{"dict 第2个写槽", `{ a=1 } -> ok, /lib/dslot2`, "/lib/dslot2", "受保护域", false},
		{"array 字面量", `[1,2] -> /sys/op/evil`, "/sys/op/evil", "受保护域", false},
		{"string.set", `string.set("X") -> /lib/pwned`, "/lib/pwned", "受保护域", false},
		{"数组元素写回槽", "a:int64 = [1,2]\n\tset(a,0,9) -> /lib/pwned", "/lib/pwned", "受保护域", false},
		// 门 3：array.go 的 set 路径分支，直接 kv.Set，不经任何解析函数
		{"set 路径分支直写签名键", `_ = set("/lib/mylib", "add", "PWN")`, "/lib/mylib.add", "受保护域", false},
		{"指针成员写", "p = \"/lib/evil\"\n\t\"PWN\" -> p.body", "/lib/evil.body", "受保护域", false},
		{"空 dict 字面量", `{} -> /lib/emptydict`, "/lib/emptydict", "受保护域", false},
		{"input 写槽", `input() -> /lib/frominput`, "/lib/frominput", "受保护域", false},
		// 域与保留键
		{"设备层任意文件写", `string.set("file") -> /dev/tty/kvlangrun/stdout/type`, "/dev/tty/kvlangrun/stdout/type", "受保护域", false},
		// 注意 base 带尾斜杠时 Member 拼出的是 "/vthread/999/" + "." + "stolen"，
		// 落点是 /vthread/999/.stolen（点号键），并不进 999 的子树 —— 所以这里用
		// 直接的绝对路径写来覆盖"真的伸进别人子树"这个形态。
		{"跨 vthread（绝对路径）", `"X" -> /vthread/999/stolen`, "/vthread/999/stolen", "不在本 vthread 的子树内", false},
		{"跨 vthread（点号兄弟键）", `_ = set("/vthread/999/", "stolen", "GOT")`, "/vthread/999/.stolen", "不在本 vthread 的子树内", false},
		// 非规范路径：art 后端自己会拒，但默认 DSN 是 redis://，那边一处路径校验
		// 都没有，这条规则是唯一防线。
		{"非规范路径（成员键拼出）", `_ = set("/a/", "/b", "V")`, "/a/./b", "不是规范路径", true},
		{"劫持别人的 ‥pc", `"HIJACK" -> /vthread/999/‥pc`, "/vthread/999/‥pc", "引擎保留键", false},
		{"关掉自己的 ‥ro", `"" -> /vthread/1/‥ro`, "/vthread/1/‥ro", "引擎保留键", false},
		{"写域根", `"X" -> /lib`, "/lib", "域根", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := newKV(t)
			vtid, err := runSrc(t, kv, "rwfunc main() -> () {\n\t"+tc.body+"\n}\n\nmain()\n")

			if err == nil || !strings.Contains(err.Error(), "PermissionError") {
				t.Fatalf("写 %s 应被拒，实得 err=%v", tc.key, err)
			}
			// 错误信息必须真的指向那个键 —— 否则 tc.key 写错了也看不出来，
			// 后面的 GetOne 断言就变成在检查一个谁也没碰过的键（空的当然空）。
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("错误信息里的键与用例声明的不一致：用例说 %q，实得 %v", tc.key, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误理由应含 %q，实得 %v", tc.want, err)
			}
			if !tc.noRead {
				if v := kvspace.GetOne(kv, tc.key); !kvspace.IsNone(v) {
					t.Fatalf("%s 不该被写入，实得 %q", tc.key, v.String())
				}
			}
			// 拒绝必须写进 ‥error/msg —— cmd/kvlang/run.go 的 reportRunError 靠它
			// 决定退出码，少了这步就是"拦住了但 exit 0"，攻击被挡住却看不出来。
			msg := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error"))
			if !strings.Contains(msg.String(), "PermissionError") {
				t.Fatalf("‥error/msg 未写入拒绝原因（会导致 exit 0 静默失败），实得 %q", msg.String())
			}
		})
	}
}

// TestRejectionIsAtomic 确认拒绝不是半执行：多写槽里有一个非法时，合法的那些
// 也不得落盘。
//
// 逐槽校验是必须的，但"边校验边写"同样不行 —— 实测修复前
// `"SRC" -> ok, /lib/slot2` 会一边报 PermissionError 一边把 ok 写成 "SRC"。
// 攻击者虽然拿不到 /lib，却仍能借一次必然失败的指令留下副作用；更要紧的是
// 用户全局键那种形态会在共享 kvspace 上活过这个已死的 vthread。
func TestRejectionIsAtomic(t *testing.T) {
	for _, tc := range []struct{ name, body, legal, illegal string }{
		{"copy 多写槽", `"SRC" -> ok, /lib/slot2`, "[0,0]/ok", "/lib/slot2"},
		{"dict 多写槽", `{ a=1 } -> ok3, /lib/dslot2`, "[0,0]/ok3", "/lib/dslot2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := newKV(t)
			vtid, err := runSrc(t, kv, "rwfunc main() -> () {\n\t"+tc.body+"\n}\n\nmain()\n")
			if err == nil {
				t.Fatal("应被拒")
			}
			if v := kvspace.GetOne(kv, keytree.VThreadAt(vtid, tc.legal)); !kvspace.IsNone(v) {
				t.Fatalf("合法槽 %s 不该落盘（拒绝必须是原子的），实得 %q", tc.legal, v.String())
			}
			if v := kvspace.GetOne(kv, tc.illegal); !kvspace.IsNone(v) {
				t.Fatalf("非法槽 %s 不该落盘，实得 %q", tc.illegal, v.String())
			}
		})
	}
}

// TestLegitimateWritesStillWork 反向钉住放行侧。这是语义变更，误伤比漏放更隐蔽：
// 漏放会被攻击者用出来，误伤只会让某个正常程序莫名其妙地失败。
func TestLegitimateWritesStillWork(t *testing.T) {
	for _, tc := range []struct{ name, body, key, want string }{
		{"局部变量", `1 + 1 -> x`, "", ""},
		{"用户全局键", `42 -> /n1`, "/n1", "42"},
		{"用户全局键成员", "{ val=7 } -> /n2\n\t7 -> /n2.val", "/n2.val", "7"},
		{"根级点号键（set 的 base 不带尾斜杠）", `_ = set("/lib", "pwned", "X")`, "/lib.pwned", "X"},
		{"kvspace /tmp 暂存区", `_ = set("/tmp/scratch", 0, 7)`, "/tmp/scratch.0", "7"},
		{"与域根同前缀的根级键", `1 -> /libfoo`, "/libfoo", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := newKV(t)
			_, err := runSrc(t, kv, "rwfunc main() -> () {\n\t"+tc.body+"\n}\n\nmain()\n")
			if err != nil {
				t.Fatalf("合法写被误伤: %v", err)
			}
			if tc.key != "" {
				if got := kvspace.GetOne(kv, tc.key).String(); got != tc.want {
					t.Fatalf("%s = %q，期望 %q", tc.key, got, tc.want)
				}
			}
		})
	}
}
