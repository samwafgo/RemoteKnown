package ruleupdate

import "testing"

// TestCompareVersionsPrerelease 验证版本比较对预发布后缀(-beta.N)的处理：
// beta 版守护进程按其基础版本参与 minAppVersion 门槛，不被同基础版本的规则误判为过低。
func TestCompareVersionsPrerelease(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.7", "1.0.5", 1},
		{"1.0.5", "1.0.7", -1},
		{"1.0.7", "1.0.7", 0},
		{"1.10", "1.9", 1},            // 按整数段比较，非字符串
		{"1.0.7-beta.1", "1.0.7", 1},  // beta 的基础版本(1.0.7)≥门槛 1.0.7 → 不被拦截
		{"1.0.6-beta.1", "1.0.7", -1}, // beta 基础版本仍低于门槛 → 拦截
		{"1.0.7-beta.1", "1.0.0", 1},  // 远高于门槛
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q)=%d, 期望 %d", c.a, c.b, got, c.want)
		}
	}

	// 门槛语义：daemon 版本 < minAppVersion 才需要升级主程序
	if CompareVersions("1.0.7-beta.1", "1.0.7") < 0 {
		t.Errorf("beta 版守护进程不应被要求 1.0.7 的规则判为版本过低")
	}
}
