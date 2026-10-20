package privacyscreen

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"RemoteKnown/internal/storage"
)

type fakeAlerter struct {
	mu     sync.Mutex
	titles []string
	lines  [][]string
}

func (f *fakeAlerter) NotifySecurityEvent(title string, lines []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titles = append(f.titles, title)
	f.lines = append(f.lines, append([]string(nil), lines...))
}

// snapshot 通知是异步发的，给它一点时间落地
func (f *fakeAlerter) snapshot() []string {
	time.Sleep(120 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.titles...)
}

func newTestManager(t *testing.T) (*Manager, *fakeAlerter) {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化临时存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	a := &fakeAlerter{}
	m := NewManager(store, a)
	t.Cleanup(m.Shutdown)
	return m, a
}

// 遮罩本来就没开时撤除，不该凭空发一条通知出去
func TestDisableWhenInactiveIsSilent(t *testing.T) {
	m, a := newTestManager(t)
	m.Disable("手动撤除")
	if got := a.snapshot(); len(got) != 0 {
		t.Errorf("未开启时撤除发出了通知: %v", got)
	}
}

// 远程结束时，触发方式为关闭也不该做任何事
func TestOnRemoteStartIgnoredWhenTriggerOff(t *testing.T) {
	m, a := newTestManager(t)
	if m.Options().Trigger != TriggerOff {
		t.Fatalf("默认触发方式应为 off")
	}
	m.OnRemoteStart()
	if got := a.snapshot(); len(got) != 0 {
		t.Errorf("触发方式为 off 时仍发出了通知: %v", got)
	}
}

// 开启通知里要能看出「那块屏幕上现在盖的是什么、怎么退出」
func TestDescribeCoversKeySettings(t *testing.T) {
	o := DefaultOptions()
	o.BgColor = "#0B3D91"
	o.Hotkey = "Ctrl+Alt+P"
	o.RequirePassword = true
	o.MaxMinutes = 30
	o.HideCursor = true

	joined := strings.Join(describe(o), "\n")
	for _, want := range []string{"#0B3D91", "Ctrl+Alt+P", "需要密码", "30 分钟", "隐藏鼠标光标"} {
		if !strings.Contains(joined, want) {
			t.Errorf("开启通知缺少 %q：\n%s", want, joined)
		}
	}

	o.Image = `D:\wall.jpg`
	o.Fit = "contain"
	joined = strings.Join(describe(o), "\n")
	if !strings.Contains(joined, "wall.jpg") || !strings.Contains(joined, "contain") {
		t.Errorf("用背景图时通知里没写清楚：\n%s", joined)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Second, "5 秒"},
		{59 * time.Second, "59 秒"},
		{90 * time.Second, "1 分 30 秒"},
		{2 * time.Minute, "2 分 0 秒"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%v) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

type fakeMaskEngine struct {
	running bool
	starts  int
}

func (e *fakeMaskEngine) active() bool               { return e.running }
func (e *fakeMaskEngine) unlockPending() bool        { return false }
func (e *fakeMaskEngine) uncoveredWindows() []string { return nil }
func (e *fakeMaskEngine) clearUnlockPending()        {}
func (e *fakeMaskEngine) stop()                      { e.running = false }
func (e *fakeMaskEngine) start(Options) error {
	e.starts++
	e.running = true
	return nil
}

func newAutoTestManager(t *testing.T) (*Manager, *fakeMaskEngine) {
	t.Helper()
	m, _ := newTestManager(t)
	eng := &fakeMaskEngine{}
	m.eng = eng
	o := DefaultOptions()
	o.Trigger = TriggerAuto
	o.AutoCountdown = true
	if err := m.SaveOptions(o); err != nil {
		t.Fatal(err)
	}
	m.OnRemoteStart()
	return m, eng
}

func assertNoAutoPending(t *testing.T, m *Manager) {
	t.Helper()
	st := m.Status()
	if st.AutoPending || st.AutoRequestID != "" || st.AutoDeadline != "" {
		t.Fatalf("请求未清除: %+v", st)
	}
}

func TestAutoCountdownDefaultsOffAndPersists(t *testing.T) {
	m, _ := newTestManager(t)
	if DefaultOptions().AutoCountdown || m.Options().AutoCountdown {
		t.Fatal("倒计时应默认关闭，旧配置缺失该键时也应关闭")
	}
	o := m.Options()
	o.AutoCountdown = true
	if err := m.SaveOptions(o); err != nil {
		t.Fatal(err)
	}
	if !m.Options().AutoCountdown {
		t.Fatal("倒计时开关未保存")
	}
	o.AutoCountdown = false
	if err := m.SaveOptions(o); err != nil {
		t.Fatal(err)
	}
	if m.Options().AutoCountdown {
		t.Fatal("倒计时开关无法关闭")
	}
}

func TestAutoWithoutCountdownDoesNotCreatePrompt(t *testing.T) {
	m, _ := newTestManager(t)
	eng := &fakeMaskEngine{}
	m.eng = eng
	o := DefaultOptions()
	o.Trigger = TriggerAuto
	if err := m.SaveOptions(o); err != nil {
		t.Fatal(err)
	}
	m.OnRemoteStart()
	assertNoAutoPending(t, m)
	if eng.starts != 1 && m.Status().LastError == "" {
		t.Fatal("倒计时关闭时应直接尝试自动开启")
	}
}

func TestAutoCancelNotifiesOnlyOnce(t *testing.T) {
	m, _ := newAutoTestManager(t)
	a := m.alert.(*fakeAlerter)
	id := m.Status().AutoRequestID
	if err := m.AutoAction("old-request", "cancel"); !errors.Is(err, ErrAutoRequestExpired) {
		t.Fatal("旧请求应被拒绝")
	}
	if err := m.AutoAction(id, "cancel"); err != nil {
		t.Fatal(err)
	}
	_ = m.AutoAction(id, "cancel")
	if got := a.snapshot(); len(got) != 1 || got[0] != "本次隐私屏自动开启已取消" {
		t.Fatalf("有效取消应仅发送一次通知: %v", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	message := strings.Join(a.lines[0], "\n")
	for _, want := range []string{"用户", "本机屏幕仍可见", "远程检测", "会话记录"} {
		if !strings.Contains(message, want) {
			t.Errorf("取消通知缺少 %q: %s", want, message)
		}
	}
}

func TestRemoteEndDoesNotSendUserCancelNotification(t *testing.T) {
	m, _ := newAutoTestManager(t)
	a := m.alert.(*fakeAlerter)
	m.OnRemoteEnd()
	if got := a.snapshot(); len(got) != 0 {
		t.Fatalf("会话结束清理不应被当作用户取消: %v", got)
	}
}

func TestAutoShownExtendsOnlyOnceAndInvalidatesOldCallback(t *testing.T) {
	m, eng := newAutoTestManager(t)
	st := m.Status()
	deadline, err := time.Parse(time.RFC3339Nano, st.AutoDeadline)
	if err != nil || !st.AutoPending || st.AutoRequestID == "" {
		t.Fatalf("初始请求无效: %+v, %v", st, err)
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > autoPromptDelay {
		t.Fatalf("初始倒计时 = %v", remaining)
	}
	// 模拟首次窗口显示前已有部分倒计时经过；旧 callback 已排队等待锁。
	oldDeadline := time.Now().Add(-time.Second)
	m.mu.Lock()
	m.autoDeadline = time.Now().Add(time.Second)
	m.mu.Unlock()
	before := time.Now()
	if err := m.AutoAction(st.AutoRequestID, "shown"); err != nil {
		t.Fatal(err)
	}
	extended := m.Status()
	newDeadline, _ := time.Parse(time.RFC3339Nano, extended.AutoDeadline)
	if newDeadline.Before(before.Add(autoPromptDelay)) {
		t.Fatalf("shown 没给足十秒: %v", newDeadline)
	}
	if err := m.AutoAction(st.AutoRequestID, "shown"); err != nil {
		t.Fatal(err)
	}
	if got := m.Status().AutoDeadline; got != extended.AutoDeadline {
		t.Fatalf("重复 shown 延长了倒计时: %s != %s", got, extended.AutoDeadline)
	}
	m.finishAuto(st.AutoRequestID, oldDeadline)
	if !m.Status().AutoPending || eng.starts != 0 {
		t.Fatal("旧 deadline 的回调处理了延期后的请求")
	}
	m.OnRemoteStart()
	if got := m.Status(); got.AutoRequestID != st.AutoRequestID || got.AutoDeadline != extended.AutoDeadline {
		t.Fatalf("同会话重复请求: %+v", got)
	}
}

func TestAutoCancelAppliesOnlyToCurrentSession(t *testing.T) {
	m, eng := newAutoTestManager(t)
	st := m.Status()
	deadline, _ := time.Parse(time.RFC3339Nano, st.AutoDeadline)
	if err := m.AutoAction("old-request", "cancel"); !errors.Is(err, ErrAutoRequestExpired) {
		t.Fatalf("旧 ID 应失效: %v", err)
	}
	if !m.Status().AutoPending {
		t.Fatal("旧 ID 取消了当前请求")
	}
	if err := m.AutoAction(st.AutoRequestID, "cancel"); err != nil {
		t.Fatal(err)
	}
	assertNoAutoPending(t, m)
	m.finishAuto(st.AutoRequestID, deadline)
	m.OnRemoteStart()
	assertNoAutoPending(t, m)
	if eng.starts != 0 {
		t.Fatal("取消后仍开启了引擎")
	}
	m.OnRemoteEnd()
	m.OnRemoteStart()
	if got := m.Status(); !got.AutoPending || got.AutoRequestID == st.AutoRequestID {
		t.Fatalf("新会话没有创建新请求: %+v", got)
	}
	if err := m.AutoAction(st.AutoRequestID, "cancel"); !errors.Is(err, ErrAutoRequestExpired) {
		t.Fatalf("旧会话请求应失效: %v", err)
	}
}

func TestAutoExpiredActionCannotCancelAndTimerCompletes(t *testing.T) {
	m, eng := newAutoTestManager(t)
	st := m.Status()
	expired := time.Now().Add(-time.Second)
	m.mu.Lock()
	m.autoTimer.Stop()
	m.autoDeadline = expired
	m.mu.Unlock()
	for _, action := range []string{"shown", "cancel"} {
		if err := m.AutoAction(st.AutoRequestID, action); !errors.Is(err, ErrAutoRequestExpired) {
			t.Fatalf("过期 %s 应失败: %v", action, err)
		}
	}
	m.finishAuto(st.AutoRequestID, expired)
	assertNoAutoPending(t, m)
	// 仅使用 fake engine；不支持平台或锁屏时应保留真实自检失败信息。
	if eng.starts != 1 && m.Status().LastError == "" {
		t.Fatal("到期后未尝试开启遮罩")
	}
	m.finishAuto(st.AutoRequestID, expired)
	m.OnRemoteStart()
	assertNoAutoPending(t, m)
	if eng.starts > 1 {
		t.Fatal("到期请求重复开启")
	}
}

func TestAutoPendingClearedByLifecycle(t *testing.T) {
	cases := []struct {
		name string
		stop func(*Manager)
	}{
		{"remote end without drop", func(m *Manager) {
			o := m.Options()
			o.DropOnEnd = false
			if err := m.SaveOptions(o); err != nil {
				t.Fatal(err)
			}
			m.OnRemoteEnd()
		}},
		{"countdown disabled", func(m *Manager) {
			o := m.Options()
			o.AutoCountdown = false
			if err := m.SaveOptions(o); err != nil {
				t.Fatal(err)
			}
		}},
		{"manual config", func(m *Manager) {
			o := m.Options()
			o.Trigger = TriggerManual
			if err := m.SaveOptions(o); err != nil {
				t.Fatal(err)
			}
		}},
		{"off config", func(m *Manager) {
			o := m.Options()
			o.Trigger = TriggerOff
			if err := m.SaveOptions(o); err != nil {
				t.Fatal(err)
			}
		}},
		{"disable", func(m *Manager) { m.Disable("test") }},
		{"manual enable", func(m *Manager) { _ = m.Enable("test") }},
		{"shutdown", func(m *Manager) { m.Shutdown() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, eng := newAutoTestManager(t)
			st := m.Status()
			deadline, _ := time.Parse(time.RFC3339Nano, st.AutoDeadline)
			tc.stop(m)
			assertNoAutoPending(t, m)
			starts := eng.starts
			m.finishAuto(st.AutoRequestID, deadline)
			if eng.starts != starts {
				t.Fatal("清除请求后旧计时器仍开启遮罩")
			}
			if tc.name == "shutdown" {
				m.OnRemoteEnd()
				m.OnRemoteStart()
				assertNoAutoPending(t, m)
				if err := m.Enable("after shutdown"); err == nil {
					t.Fatal("退出后仍允许开启")
				}
			}
		})
	}
}

// 颜色是 #RRGGBB，而 Win32 的 COLORREF 是 0x00BBGGRR，字节序相反，容易写反
func TestParseHexColorByteOrder(t *testing.T) {
	got, err := parseHexColor("#0A2A43")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	const want = 0x00432A0A
	if got != want {
		t.Errorf("parseHexColor(#0A2A43) = 0x%06X, 期望 0x%06X（R 和 B 要互换）", got, want)
	}
	if _, err := parseHexColor("nope"); err == nil {
		t.Error("非法颜色应当报错")
	}
}
