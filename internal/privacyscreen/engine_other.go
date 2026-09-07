//go:build !windows

package privacyscreen

import "errors"

// 隐私屏依赖 Windows 的 SetWindowDisplayAffinity(WDA_EXCLUDEFROMCAPTURE)，
// 其他平台只提供空实现，保证整个模块仍可编译。

const minBuild uint32 = 19041

var errUnsupported = errors.New("隐私屏仅支持 Windows")

func platformSupported() (bool, uint32) { return false, 0 }
func desktopLocked() bool               { return false }
func lockWorkstation() error            { return errUnsupported }
func resetCursorsOnStartup()            {}

type engine struct {
	onFail    func(string)
	onStopped func(string)
}

func newEngine() *engine                     { return &engine{} }
func (e *engine) active() bool               { return false }
func (e *engine) unlockPending() bool        { return false }
func (e *engine) uncoveredWindows() []string { return nil }
func (e *engine) clearUnlockPending()        {}
func (e *engine) stop()                      {}

func (e *engine) start(o Options) error { return errUnsupported }
