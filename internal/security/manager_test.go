package security

import (
	"path/filepath"
	"sync"
	"testing"

	"RemoteKnown/internal/storage"
)

// fakeAlerter 记录外发的安全事件，用来断言"该告警的时候确实告警了"。
type fakeAlerter struct {
	mu     sync.Mutex
	titles []string
	code   string
}

func (f *fakeAlerter) NotifySecurityEvent(title string, lines []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titles = append(f.titles, title)
}

func (f *fakeAlerter) SendResetCode(code string, ttlMinutes int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code = code
	return nil
}

func (f *fakeAlerter) sentCode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.code
}

func newTestManager(t *testing.T) (*Manager, *fakeAlerter) {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化临时存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	alerter := &fakeAlerter{}
	return NewManager(store, alerter), alerter
}

func TestSetupAndVerify(t *testing.T) {
	m, _ := newTestManager(t)

	if m.Enabled() {
		t.Error("刚初始化时不应处于启用状态")
	}

	recovery, err := m.Setup("s3cret")
	if err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}
	if recovery == "" {
		t.Fatal("Setup 应返回恢复码")
	}
	if !m.Enabled() {
		t.Error("Setup 之后应处于启用状态")
	}
	if !m.SilentLocal() {
		t.Error("默认应开启本地通知静默")
	}

	token, err := m.Verify("s3cret", "退出程序")
	if err != nil {
		t.Fatalf("正确密码应通过: %v", err)
	}
	if !m.ValidateToken(token) {
		t.Error("刚发出的令牌应当有效")
	}
	if m.ValidateToken("deadbeef") {
		t.Error("伪造的令牌不应通过")
	}

	if _, err := m.Setup("another"); err == nil {
		t.Error("已设置密码后重复 Setup 应报错")
	}
}

func TestVerifyFailuresLockOut(t *testing.T) {
	m, alerter := newTestManager(t)
	if _, err := m.Setup("s3cret"); err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}

	// 前 4 次：返回剩余次数，不锁定
	for i := 1; i <= maxAttempts-1; i++ {
		_, err := m.Verify("wrong", "退出程序")
		wrong, ok := err.(*ErrWrongPassword)
		if !ok {
			t.Fatalf("第 %d 次错误密码应返回 ErrWrongPassword，实得 %v", i, err)
		}
		if want := maxAttempts - i; wrong.AttemptsLeft != want {
			t.Errorf("第 %d 次剩余次数 = %d，期望 %d", i, wrong.AttemptsLeft, want)
		}
	}

	// 第 5 次：触发锁定
	if _, err := m.Verify("wrong", "退出程序"); err == nil {
		t.Fatal("第 5 次错误应触发锁定")
	} else if _, ok := err.(*ErrLocked); !ok {
		t.Fatalf("第 5 次错误应返回 ErrLocked，实得 %T", err)
	}

	// 锁定期内即便密码正确也要挡住
	if _, err := m.Verify("s3cret", "退出程序"); err == nil {
		t.Error("锁定期内正确密码也应被拒绝")
	} else if _, ok := err.(*ErrLocked); !ok {
		t.Errorf("锁定期内应返回 ErrLocked，实得 %T", err)
	}

	if !m.State().Locked {
		t.Error("State 应报告处于锁定状态")
	}

	alerter.mu.Lock()
	n := len(alerter.titles)
	alerter.mu.Unlock()
	if n == 0 {
		t.Error("失败与锁定都应对外告警")
	}
}

func TestFailureCountSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	store, err := storage.NewStorage(dbPath)
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	m := NewManager(store, &fakeAlerter{})
	if _, err := m.Setup("s3cret"); err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}
	for i := 0; i < 3; i++ {
		m.Verify("wrong", "退出程序")
	}
	store.Close()

	// 重开一次，模拟进程被杀掉后重启：失败计数必须还在
	store2, err := storage.NewStorage(dbPath)
	if err != nil {
		t.Fatalf("重新打开存储失败: %v", err)
	}
	defer store2.Close()

	m2 := NewManager(store2, &fakeAlerter{})
	if got := m2.State().FailCount; got != 3 {
		t.Errorf("重启后失败计数 = %d，期望 3（不能靠杀进程清零）", got)
	}
	if _, err := m2.Verify("wrong", "退出程序"); err == nil {
		t.Fatal("重启后第 4 次错误仍应失败")
	} else if wrong, ok := err.(*ErrWrongPassword); !ok || wrong.AttemptsLeft != 1 {
		t.Errorf("重启后应接着数，实得 %v", err)
	}
}

func TestResetWithRecoveryCode(t *testing.T) {
	m, _ := newTestManager(t)
	recovery, err := m.Setup("s3cret")
	if err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}

	// 先把自己锁死，确认重置能解开
	for i := 0; i < maxAttempts; i++ {
		m.Verify("wrong", "退出程序")
	}
	if !m.State().Locked {
		t.Fatal("应处于锁定状态")
	}

	if err := m.ConfirmReset("", "RK-BAD0-BAD0-BAD0", "newpass"); err == nil {
		t.Error("错误的恢复码不应通过")
	}
	if err := m.ConfirmReset("", recovery, "newpass"); err != nil {
		t.Fatalf("正确恢复码重置失败: %v", err)
	}
	if m.State().Locked {
		t.Error("重置后应解除锁定")
	}

	if _, err := m.Verify("newpass", "退出程序"); err != nil {
		t.Errorf("新密码应可用: %v", err)
	}
	if _, err := m.Verify("s3cret", "退出程序"); err == nil {
		t.Error("旧密码应失效")
	}

	// 恢复码一次性：用过之后换发新的，旧的不能再用
	if fresh := m.TakeFreshRecovery(); fresh == "" {
		t.Error("用恢复码重置后应换发新的恢复码")
	} else if fresh == recovery {
		t.Error("换发的恢复码不应与旧的相同")
	}
	if m.TakeFreshRecovery() != "" {
		t.Error("新恢复码只能取一次")
	}
}

func TestResetWithCode(t *testing.T) {
	m, alerter := newTestManager(t)
	if _, err := m.Setup("s3cret"); err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}

	if err := m.RequestReset(); err != nil {
		t.Fatalf("RequestReset 失败: %v", err)
	}
	code := alerter.sentCode()
	if len(code) != 6 {
		t.Fatalf("验证码应为 6 位，实得 %q", code)
	}

	if err := m.ConfirmReset("000000", "", "newpass"); err == nil && code != "000000" {
		t.Error("错误验证码不应通过")
	}
	if err := m.ConfirmReset(code, "", "newpass"); err != nil {
		t.Fatalf("正确验证码重置失败: %v", err)
	}
	if _, err := m.Verify("newpass", "退出程序"); err != nil {
		t.Errorf("新密码应可用: %v", err)
	}

	// 验证码用过即废
	if err := m.ConfirmReset(code, "", "again123"); err == nil {
		t.Error("验证码不应能重复使用")
	}
}

func TestChangeAndDisable(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Setup("s3cret"); err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}

	if err := m.Change("wrong", "newpass"); err == nil {
		t.Error("旧密码不对时不应允许修改")
	}
	if err := m.Change("s3cret", "abc"); err == nil {
		t.Error("过短的新密码应被拒绝")
	}
	if err := m.Change("s3cret", "newpass"); err != nil {
		t.Fatalf("修改密码失败: %v", err)
	}

	token, err := m.Verify("newpass", "退出程序")
	if err != nil {
		t.Fatalf("新密码应可用: %v", err)
	}

	if err := m.Disable("wrong"); err == nil {
		t.Error("密码不对时不应允许关闭保护")
	}
	if err := m.Disable("newpass"); err != nil {
		t.Fatalf("关闭保护失败: %v", err)
	}
	if m.Enabled() {
		t.Error("关闭后不应再处于启用状态")
	}
	if m.ValidateToken(token) {
		t.Error("关闭保护后旧令牌应作废")
	}
	if m.SilentLocal() {
		t.Error("未启用保护时不应静默本地通知")
	}
}

func TestSetOptions(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Setup("s3cret"); err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}

	off := false
	if err := m.SetOptions(&off, nil); err != nil {
		t.Fatalf("SetOptions 失败: %v", err)
	}
	st := m.State()
	if st.SilentLocal {
		t.Error("静默应已关闭")
	}
	if !st.LockTray {
		t.Error("只传了静默，托盘开关不应被改动")
	}
	if m.SilentLocal() {
		t.Error("SilentLocal() 应跟随开关")
	}
}
