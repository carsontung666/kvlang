package keytree

import "testing"

// 落点校验的纯路径用例。端到端行为见 rwir/dispatch/delegate_test.go。
func TestCheckWriteKey(t *testing.T) {
	deny := map[string]string{
		"/vthread/1/‥returnpc":     "引擎保留键",
		"/vthread/1/[0,0]/‥ro":     "引擎保留键",
		"/lib/main/[1,0]":          "受保护域",
		"/lib":                     "域根",
		"/sys":                     "域根",
		"/dev":                     "域根",
		"/vthread":                 "域根",
		"/sys/op/evil/0":           "受保护域",
		"/dev/tty/x/stdout/detail": "受保护域",
		"/vthread/999/r":           "越出本 vthread",
		// 非规范路径：纯前缀比较对它们无效，不能指望 kvspace 兜底
		"/foo/../lib/main/x":  "规范",
		"//lib/x":             "规范",
		"/./lib/x":            "规范",
		"/vthread/1/../999/x": "规范",
		"/vthread/1/x/":       "规范",
		"":                    "绝对路径",
		"relative/x":          "绝对路径",
	}
	for key, want := range deny {
		err := CheckWriteKey("1", key)
		if err == nil {
			t.Errorf("%q 应被拒绝", key)
		} else if !contains(err.Error(), want) {
			t.Errorf("%q 的拒绝原因应含 %q，实得 %v", key, want, err)
		}
	}
	for _, ok := range []string{"/vthread/1/[0,0]/r", "/vthread/1/x", "/n1", "/mydata/x", "/libx/y", "/system/y"} {
		if err := CheckWriteKey("1", ok); err != nil {
			t.Errorf("合法写槽 %q 被误拒: %v", ok, err)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
