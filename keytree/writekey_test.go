package keytree

import (
	"strings"
	"testing"
)

// TestCheckWriteKeyDenies 逐条钉住拒绝规则。
//
// 这必须是**纯函数单测**，端到端矩阵替代不了：
//   - 「非规范路径」那条在 art:// 下永远触发不到 —— art 后端的
//     validateAbsolutePath 自己就拒 . / .. / //，所以端到端跑只能看到 kvspace
//     的错误，看不出我们这条规则在不在。而默认 DSN 是 redis://，redis 后端
//     一处路径校验都没有，那里这条规则是唯一防线。
//   - 校验函数按约定不碰 kvspace（art 的 Get/List 对非规范路径直接 panic，
//     校验里一旦调它们，同样的输入就从「安全拒绝」退化成「拒绝服务」），
//     所以它天然适合纯函数测试。
func TestCheckWriteKeyDenies(t *testing.T) {
	const vtid = "7"
	for _, tc := range []struct{ name, key, want string }{
		// 非绝对
		{"空键", "", "不是绝对路径"},
		{"相对路径", "foo/bar", "不是绝对路径"},

		// 非规范：art 会自己拒，redis 不会 —— 这几条是 redis 下的唯一防线
		{"父目录穿越", "/vthread/7/../lib/x", "不是规范路径"},
		{"双斜杠", "//lib/x", "不是规范路径"},
		{"当前目录段", "/./lib/x", "不是规范路径"},
		{"中段穿越", "/a/b/../../lib/x", "不是规范路径"},
		{"尾斜杠", "/vthread/7/x/", "不是规范路径"},
		{"仅根", "/", "不是规范路径"},

		// 引擎保留键：任意位置，且必须压过"这是我自己的 vthread"
		{"别人的 ‥pc", "/vthread/999/‥pc", "引擎保留键"},
		{"自己的 ‥pc", "/vthread/7/‥pc", "引擎保留键"},
		{"自己的 ‥ro", "/vthread/7/‥ro", "引擎保留键"},
		{"自己的 ‥returnpc", "/vthread/7/[0,0]/‥returnpc", "引擎保留键"},
		{"自己的 ‥wparam", "/vthread/7/[0,0]/‥wparam/x", "引擎保留键"},
		{"自己的 ‥lib", "/vthread/7/[0,0]/‥lib", "引擎保留键"},
		{"vthread 序列号", "/vthread/‥seq", "引擎保留键"},
		{"用户全局区的 ‥", "/‥sneaky", "引擎保留键"},
		{"深层 ‥ 段", "/a/b/‥c/d", "引擎保留键"},

		// 受保护域
		{"代码区", "/lib/pwned", "受保护域"},
		{"代码区深层", "/lib/mylib.add", "受保护域"},
		{"后端注册表(旧路径)", "/sys/op/evil/0", "受保护域"},
		{"后端注册表", "/sys/rwir-backend/evil/op/llm.chat", "受保护域"},
		{"委托任务对象", "/sys/task/1-1.status", "受保护域"},
		{"设备层", "/dev/tty/kvlangrun/stdout/detail", "受保护域"},
		// /done 是委托完成信号所在的命名空间，整域归引擎。纵深防御：树键写进去
		// 唤不醒任何 Watch（Notify 队列在树外），但程序往这里写没有正当用途。
		{"完成信号", "/done/rwir/rwir:fake:1:1", "受保护域"},
		{"完成信号域根", "/done", "域根"},

		// 域根本身
		{"域根 /lib", "/lib", "域根"},
		{"域根 /sys", "/sys", "域根"},
		{"域根 /dev", "/dev", "域根"},
		{"域根 /vthread", "/vthread", "域根"},

		// vthread 边界
		{"别的 vthread", "/vthread/999/stolen", "不在本 vthread 的子树内"},
		{"别的 vthread 根", "/vthread/999", "不在本 vthread 的子树内"},
		{"自己的帧根", "/vthread/7", "本 vthread 的帧根"},
		{"/vthread 下的兄弟标量键", "/vthread/999.s", "不在本 vthread 的子树内"},
		// 自己 vtid 的点号兄弟键同样被拒 —— 这是"整域归引擎"规则的代价，
		// 措辞必须是"不在子树内"而不是"越出本 vthread"（程序哪儿也没越）
		{"自己 vtid 的点号兄弟键", "/vthread/7.note", "不在本 vthread 的子树内"},
		{"vtid 前缀相同但不同线程", "/vthread/70/x", "不在本 vthread 的子树内"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckWriteKey(vtid, tc.key)
			if err == nil {
				t.Fatalf("CheckWriteKey(%q,%q) = nil，应被拒绝", vtid, tc.key)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应含 %q，实得 %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "PermissionError") {
				t.Fatalf("错误应带 PermissionError 前缀，实得 %v", err)
			}
		})
	}
}

// TestCheckWriteKeyAllows 反向钉住放行集合。误伤比漏放更难被发现 ——
// 漏放会被攻击者用出来，误伤只会让某个正常程序莫名其妙地失败。
func TestCheckWriteKeyAllows(t *testing.T) {
	const vtid = "7"
	for _, key := range []string{
		// 自己 vthread 的槽位
		"/vthread/7/x",
		"/vthread/7/[0,0]/local",
		"/vthread/7/[0,0]/[1,0]/nested",
		"/vthread/7/[0,0]/d.member",
		// 用户全局键
		"/n1",
		"/n1.val",
		"/last_sum",
		"/tmp/pt.0",
		"/tmp/kvhas_test.data",
		// 与域根同前缀但不在域内的根级键（keytree.Member 拼的是点号）
		"/lib.pwned",
		"/libfoo",
		"/system",
		"/device",
		"/vthreadX",
		// 段里含点、含方括号都不是保留形态
		"/a.b.c",
		"/x[0]",
	} {
		if err := CheckWriteKey(vtid, key); err != nil {
			t.Errorf("CheckWriteKey(%q,%q) 不该拒绝，实得 %v", vtid, key, err)
		}
	}
}

// TestCheckWriteKeyEmptyVtid 确认拿不到 vtid 时按最保守处理：
// 整个 /vthread 域一律拒。宁可让调用方报错，也不能因为 vtid 解析失败就放行。
func TestCheckWriteKeyEmptyVtid(t *testing.T) {
	for _, key := range []string{"/vthread/1/x", "/vthread/7/[0,0]/y", "/vthread/999/z"} {
		if err := CheckWriteKey("", key); err == nil {
			t.Errorf("vtid 为空时 CheckWriteKey(%q) 应拒绝", key)
		}
	}
	// 用户全局键不受影响
	if err := CheckWriteKey("", "/n1"); err != nil {
		t.Errorf("vtid 为空不该影响用户全局键，实得 %v", err)
	}
}

// TestCheckWriteKeyHomoglyphs 确认只有真正的 U+2025 才被当引擎保留前缀。
// ASCII 的 ".." 与 U+2024 的 "․" 是**不同的键**，拦不拦都劫持不到任何东西；
// 把它们一并拦掉反而会误伤合法命名。这里钉住边界，防止有人"顺手加固"。
func TestCheckWriteKeyHomoglyphs(t *testing.T) {
	if RuntimeMemberSep != "‥" {
		t.Fatalf("RuntimeMemberSep 变了（%q），本用例的前提不再成立", RuntimeMemberSep)
	}
	// U+2025 开头 → 拒
	if err := CheckWriteKey("7", "/vthread/7/‥pc"); err == nil {
		t.Error("U+2025 开头的段应被拒")
	}
	// 单点前缀 U+2024、ASCII 双点开头的普通名字 → 放行（它们是别的键）
	for _, key := range []string{"/vthread/7/․pc", "/vthread/7/..pc", "/vthread/7/.pc"} {
		if err := CheckWriteKey("7", key); err != nil {
			t.Errorf("%q 是普通用户键，不该拒绝，实得 %v", key, err)
		}
	}
}
