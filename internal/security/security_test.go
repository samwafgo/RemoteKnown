package security

import (
	"strings"
	"testing"
)

func TestHashRoundTrip(t *testing.T) {
	encoded, err := hashPassword("hunter2")
	if err != nil {
		t.Fatalf("hashPassword 失败: %v", err)
	}
	if !strings.HasPrefix(encoded, "pbkdf2-sha256$") {
		t.Fatalf("哈希格式不对: %s", encoded)
	}
	if !matchHash(encoded, "hunter2") {
		t.Error("正确密码应当匹配")
	}
	if matchHash(encoded, "hunter3") {
		t.Error("错误密码不应匹配")
	}
}

func TestHashUsesRandomSalt(t *testing.T) {
	a, _ := hashPassword("same")
	b, _ := hashPassword("same")
	if a == b {
		t.Error("相同密码两次哈希应因随机盐而不同")
	}
}

func TestMatchHashRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext",
		"pbkdf2-sha256$abc$xx$yy",      // 迭代次数不是数字
		"bcrypt$1$aaaa$bbbb",           // 算法不认识
		"pbkdf2-sha256$1000$!!!$bbbb",  // 盐不是合法 base64
		"pbkdf2-sha256$1000$YWJj$!!!!", // 哈希不是合法 base64
		"pbkdf2-sha256$1000$YWJj",      // 段数不对
	} {
		if matchHash(bad, "whatever") {
			t.Errorf("畸形哈希不应通过校验: %q", bad)
		}
	}
}

func TestNewRecoveryCodeFormat(t *testing.T) {
	code, err := newRecoveryCode()
	if err != nil {
		t.Fatalf("newRecoveryCode 失败: %v", err)
	}
	if len(code) != 17 { // RK- + 4 + - + 4 + - + 4
		t.Fatalf("恢复码长度不对: %q", code)
	}
	if !strings.HasPrefix(code, "RK-") {
		t.Fatalf("恢复码缺少前缀: %q", code)
	}
	for _, ch := range strings.ReplaceAll(strings.TrimPrefix(code, "RK-"), "-", "") {
		if !strings.ContainsRune(recoveryAlphabet, ch) {
			t.Errorf("恢复码含有字母表之外的字符 %q: %s", ch, code)
		}
	}
}

func TestNormalizeRecovery(t *testing.T) {
	const want = "RK-A2B3-C4D5-E6F7"
	cases := []string{
		"RK-A2B3-C4D5-E6F7",
		"rk-a2b3-c4d5-e6f7",
		"  RK-A2B3-C4D5-E6F7  ",
		"RKA2B3C4D5E6F7",
		"A2B3C4D5E6F7",
		"a2b3 c4d5 e6f7",
	}
	for _, in := range cases {
		if got := normalizeRecovery(in); got != want {
			t.Errorf("normalizeRecovery(%q) = %q, 期望 %q", in, got, want)
		}
	}
	for _, in := range []string{"", "RK-短", "A2B3C4D5E6F", "A2B3C4D5E6F78"} {
		if got := normalizeRecovery(in); got != "" {
			t.Errorf("normalizeRecovery(%q) 应当判为无效，实得 %q", in, got)
		}
	}
}
