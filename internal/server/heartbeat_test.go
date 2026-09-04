package server

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"RemoteKnown/internal/security"
	"RemoteKnown/internal/storage"
)

// fakeAlerter 记录安全事件标题，用来断言看门狗到底报没报警。
type fakeAlerter struct {
	mu     sync.Mutex
	titles []string
}

func (f *fakeAlerter) NotifySecurityEvent(title string, lines []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titles = append(f.titles, title)
}

func (f *fakeAlerter) SendResetCode(code string, ttlMinutes int) error { return nil }

// titlesAfter 等待告警落地（Alert 是异步 go 出去的），最多等 1 秒。
func (f *fakeAlerter) titlesAfter(want int) []string {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.titles)
		f.mu.Unlock()
		if n >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.titles...)
}

func newHeartbeatServer(t *testing.T, withPassword bool) (*Server, *fakeAlerter) {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化临时存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	alerter := &fakeAlerter{}
	sec := security.NewManager(store, alerter)
	if withPassword {
		if _, err := sec.Setup("s3cret"); err != nil {
			t.Fatalf("Setup 失败: %v", err)
		}
		// Setup 自己会发一条"密码保护已启用"，先等它落地再清掉，
		// 免得和后面的断言混在一起
		alerter.titlesAfter(1)
		alerter.mu.Lock()
		alerter.titles = nil
		alerter.mu.Unlock()
	}

	srv := NewServer(nil, store, nil, sec)
	return srv, alerter
}

// rewindHeartbeat 把上次心跳时间往前拨，模拟界面已经失联了这么久。
func (s *Server) rewindHeartbeat(d time.Duration) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	s.lastSeen = time.Now().Add(-d)
}

func TestHeartbeatAlertsWhenUIKilled(t *testing.T) {
	srv, alerter := newHeartbeatServer(t, true)

	srv.touchHeartbeat()
	srv.checkUIHeartbeat()
	if got := alerter.titlesAfter(0); len(got) != 0 {
		t.Fatalf("心跳正常时不应告警，实得 %v", got)
	}

	srv.rewindHeartbeat(uiHeartbeatTimeout + time.Second)
	srv.checkUIHeartbeat()

	got := alerter.titlesAfter(1)
	if len(got) != 1 || got[0] != "界面进程已被终止" {
		t.Fatalf("界面失联应告警一次，实得 %v", got)
	}

	// 再检查几次也不能重复刷
	srv.rewindHeartbeat(uiHeartbeatTimeout + 10*time.Second)
	srv.checkUIHeartbeat()
	srv.checkUIHeartbeat()
	if got := alerter.titlesAfter(2); len(got) != 1 {
		t.Errorf("同一次失联只应告警一次，实得 %v", got)
	}
}

func TestHeartbeatRecoveryAlert(t *testing.T) {
	srv, alerter := newHeartbeatServer(t, true)

	srv.touchHeartbeat()
	srv.rewindHeartbeat(uiHeartbeatTimeout + time.Second)
	srv.checkUIHeartbeat()
	alerter.titlesAfter(1)

	// 界面回来了：补一条恢复通知，并且允许下一次失联再次告警
	srv.touchHeartbeat()
	got := alerter.titlesAfter(2)
	if len(got) != 2 || got[1] != "界面进程已恢复" {
		t.Fatalf("界面恢复应补一条通知，实得 %v", got)
	}

	srv.rewindHeartbeat(uiHeartbeatTimeout + time.Second)
	srv.checkUIHeartbeat()
	if got := alerter.titlesAfter(3); len(got) != 3 || got[2] != "界面进程已被终止" {
		t.Errorf("恢复之后再次失联应重新告警，实得 %v", got)
	}
}

func TestHeartbeatSilentWhenProtectionOff(t *testing.T) {
	srv, alerter := newHeartbeatServer(t, false)

	srv.touchHeartbeat()
	srv.rewindHeartbeat(uiHeartbeatTimeout + time.Second)
	srv.checkUIHeartbeat()

	if got := alerter.titlesAfter(0); len(got) != 0 {
		t.Errorf("未启用密码保护时关掉界面属正常操作，不应告警，实得 %v", got)
	}
}

func TestHeartbeatSilentBeforeFirstBeat(t *testing.T) {
	srv, alerter := newHeartbeatServer(t, true)

	// 守护进程刚起来、界面还没连上过，不能凭空报"界面被杀"
	srv.checkUIHeartbeat()
	srv.checkUIHeartbeat()

	if got := alerter.titlesAfter(0); len(got) != 0 {
		t.Errorf("界面从未连上过时不应告警，实得 %v", got)
	}
}

func TestWatchdogStopsOnServerStop(t *testing.T) {
	srv, _ := newHeartbeatServer(t, true)

	done := make(chan struct{})
	go func() {
		srv.watchUIHeartbeat()
		close(done)
	}()

	srv.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Stop() 之后看门狗应当退出")
	}

	srv.Stop() // 重复调用不应 panic
}
