//go:build windows

package privacyscreen

import "testing"

// 只测试键态与放行策略，不创建遮罩或安装系统钩子。
func TestHotkeyRequiresMainKey(t *testing.T) {
	hk := parseHotkey("Ctrl+Alt+P")
	keys := map[uint32]bool{0x11: true, 0x12: true}
	down := func(key uint32) bool { return keys[key] }
	if hk.pressed(down) {
		t.Fatal("仅按 Ctrl+Alt 不应触发 Ctrl+Alt+P")
	}
	keys['P'] = true
	if !hk.pressed(down) {
		t.Fatal("完整组合键应触发")
	}
	keys[0x10] = true
	if hk.pressed(down) {
		t.Fatal("额外按 Shift 不应匹配")
	}
}

func TestHookInputAllowsUnlockAndRemoteInput(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unlocking bool
		flags     uint32
		injected  uint32
		block     bool
	}{
		{"local keyboard", false, 0, 0x10, true},
		{"unlock keyboard", true, 0, 0x10, false},
		{"remote keyboard", false, 0x10, 0x10, false},
		{"local mouse", false, 0, 0x01, true},
		{"unlock mouse", true, 0, 0x01, false},
		{"remote mouse", false, 0x01, 0x01, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockHookInput(tc.unlocking, tc.flags, tc.injected); got != tc.block {
				t.Fatalf("block = %v, want %v", got, tc.block)
			}
		})
	}
}

func TestHookHotkeyTracksSuppressedModifiers(t *testing.T) {
	old := hookKeys
	t.Cleanup(func() { hookKeys = old })
	hookKeys = [256]bool{}
	hookKeys[0xA2] = true // 左 Ctrl
	hookKeys[0xA5] = true // 右 Alt
	if !parseHotkey("Ctrl+Alt+P").modifiersMatch(hookKeyDown) {
		t.Fatal("被吞掉的左右修饰键也应识别，无需依赖系统异步键态")
	}
	hookKeys[0xA2] = false
	if parseHotkey("Ctrl+Alt+P").modifiersMatch(hookKeyDown) {
		t.Fatal("释放 Ctrl 后不应继续匹配")
	}
}

func TestUnlockPendingControlsHookInput(t *testing.T) {
	e := newEngine()
	e.markUnlockPending()
	if blockHookInput(e.unlockPending(), 0, 0x10) {
		t.Fatal("等待解锁时应放行输入")
	}
	e.clearUnlockPending()
	if !blockHookInput(e.unlockPending(), 0, 0x10) {
		t.Fatal("取消解锁后应恢复本地输入屏蔽")
	}
}
