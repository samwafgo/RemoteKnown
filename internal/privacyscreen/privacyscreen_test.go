package privacyscreen

import (
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
}

func (f *fakeAlerter) NotifySecurityEvent(title string, lines []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titles = append(f.titles, title)
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
	return NewManager(store, a), a
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
