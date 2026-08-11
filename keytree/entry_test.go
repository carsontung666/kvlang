package keytree

import "testing"

// TestSysTask 钉住委托任务对象的键布局。VM 与执行器都过这个函数，光靠它们
// 互相对得上说明不了什么 —— examples/delegate/fake_backend.py 是**手拼**
// "/sys/task/{id}.status" 的，格式一变它就默默写到别处去。
// 点号键族（而非斜杠子项）是刻意的：handle 可用现有的 at(h,"field") 解引用。
func TestSysTask(t *testing.T) {
	for _, tc := range []struct{ id, field, want string }{
		{"1-2", "status", "/sys/task/1-2.status"},
		{"1-2", "done", "/sys/task/1-2.done"},
		{"1000-13", "status", "/sys/task/1000-13.status"},
	} {
		if got := SysTask(tc.id, tc.field); got != tc.want {
			t.Errorf("SysTask(%q,%q) = %q，期望 %q", tc.id, tc.field, got, tc.want)
		}
	}
}

// TestFuncKey 钉住调用点算子名 → /lib 签名键的映射。
// layout.HandleCall / layout.Bootstrap / dispatch.delegatedSig 三处共用它，
// 任何一处对不上都会变成"函数明明装载了却报 NameError"。
func TestFuncKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"add", "/lib/add"},
		{"mylib.add", "/lib/mylib.add"},
		{"llm.chat", "/lib/llm.chat"},
		{"a.b.c", "/lib/a.b.c"},
		{"a/b.c", "/lib/a/b.c"}, // 嵌套 lib 的包名用斜杠
		{"/lib/mylib.add", "/lib/mylib.add"},
		{"/lib/plain", "/lib/plain"},
		{"/lib/a.b.c", "/lib/a.b.c"},
	} {
		if got := FuncKey(tc.in); got != tc.want {
			t.Errorf("FuncKey(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestFuncKeyIsIdempotent 确认对已经是 /lib 键的输入再跑一次不会变形 ——
// HandleCall 收到的算子名有时已被上游归一化过。
func TestFuncKeyIsIdempotent(t *testing.T) {
	for _, in := range []string{"add", "mylib.add", "a.b.c", "/lib/x.y"} {
		once := FuncKey(in)
		if twice := FuncKey(once); twice != once {
			t.Errorf("FuncKey 不幂等：%q → %q → %q", in, once, twice)
		}
	}
}
