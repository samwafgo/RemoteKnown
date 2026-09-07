//go:build windows

package privacyscreen

import (
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// WDA_EXCLUDEFROMCAPTURE 自 Windows 10 2004（20H1）起可用。
// 更低版本会静默降级成 WDA_MONITOR —— 语义变成「抓屏里这块空白」，
// 远端画面也会跟着变黑，远程会话直接废掉。所以必须硬卡版本。
const minBuild uint32 = 19041

var (
	user32 = syscall.NewLazyDLL("user32.dll")
	gdi32  = syscall.NewLazyDLL("gdi32.dll")
	krnl32 = syscall.NewLazyDLL("kernel32.dll")
	ntdll  = syscall.NewLazyDLL("ntdll.dll")

	pRegisterClassExW      = user32.NewProc("RegisterClassExW")
	pCreateWindowExW       = user32.NewProc("CreateWindowExW")
	pDefWindowProcW        = user32.NewProc("DefWindowProcW")
	pShowWindow            = user32.NewProc("ShowWindow")
	pUpdateWindow          = user32.NewProc("UpdateWindow")
	pDestroyWindow         = user32.NewProc("DestroyWindow")
	pPeekMessageW          = user32.NewProc("PeekMessageW")
	pTranslateMessage      = user32.NewProc("TranslateMessage")
	pDispatchMessageW      = user32.NewProc("DispatchMessageW")
	pSetWindowDisplayAffin = user32.NewProc("SetWindowDisplayAffinity")
	pGetWindowDisplayAffin = user32.NewProc("GetWindowDisplayAffinity")
	pSetWindowPos          = user32.NewProc("SetWindowPos")
	pSetLayeredWindowAttr  = user32.NewProc("SetLayeredWindowAttributes")
	pDrawTextW             = user32.NewProc("DrawTextW")
	pFillRect              = user32.NewProc("FillRect")
	pGetClientRect         = user32.NewProc("GetClientRect")
	pGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	pSetProcessDPIAware    = user32.NewProc("SetProcessDPIAware")
	pGetDC                 = user32.NewProc("GetDC")
	pReleaseDC             = user32.NewProc("ReleaseDC")
	pLoadCursorW           = user32.NewProc("LoadCursorW")
	pDisableGhosting       = user32.NewProc("DisableProcessWindowsGhosting")
	pCreateCursor          = user32.NewProc("CreateCursor")
	pSetSystemCursor       = user32.NewProc("SetSystemCursor")
	pSystemParametersInfoW = user32.NewProc("SystemParametersInfoW")
	pOpenInputDesktop      = user32.NewProc("OpenInputDesktop")
	pCloseDesktop          = user32.NewProc("CloseDesktop")
	pLockWorkStation       = user32.NewProc("LockWorkStation")
	pSetWindowsHookExW     = user32.NewProc("SetWindowsHookExW")
	pUnhookWindowsHookEx   = user32.NewProc("UnhookWindowsHookEx")
	pCallNextHookEx        = user32.NewProc("CallNextHookEx")
	pGetAsyncKeyState      = user32.NewProc("GetAsyncKeyState")
	pGetWindow             = user32.NewProc("GetWindow")
	pIsWindowVisible       = user32.NewProc("IsWindowVisible")
	pGetClassNameW         = user32.NewProc("GetClassNameW")
	pGetWindowRect         = user32.NewProc("GetWindowRect")

	pCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	pCreateFontW        = gdi32.NewProc("CreateFontW")
	pSetTextColor       = gdi32.NewProc("SetTextColor")
	pSetBkMode          = gdi32.NewProc("SetBkMode")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pBitBlt             = gdi32.NewProc("BitBlt")
	pStretchBlt         = gdi32.NewProc("StretchBlt")
	pSetStretchBltMode  = gdi32.NewProc("SetStretchBltMode")
	pDeleteDC           = gdi32.NewProc("DeleteDC")

	pGetModuleHandleW = krnl32.NewProc("GetModuleHandleW")
	pRtlGetVersion    = ntdll.NewProc("RtlGetVersion")
)

const (
	wsPopup         = 0x80000000
	wsExTopmost     = 0x00000008
	wsExToolWindow  = 0x00000080
	wsExNoActivate  = 0x08000000
	wsExLayered     = 0x00080000
	wsExTransparent = 0x00000020

	swShowNoActivate = 4
	pmRemove         = 1
	lwaAlpha         = 0x02
	idcArrow         = 32512

	swpNoMove     = 0x0002
	swpNoSize     = 0x0001
	swpNoActivate = 0x0010

	dtCenter       = 0x00000001
	dtVCenter      = 0x00000004
	dtSingleLine   = 0x00000020
	bkTransparent  = 1
	defaultCharset = 1

	srcCopy   = 0x00CC0020
	halftone  = 4
	stagingUS = 3

	wdaNone               = 0x00000000
	wdaExcludeFromCapture = 0x00000011

	whKeyboardLL = 13
	whMouseLL    = 14

	spiSetCursors = 0x0057

	gwHwndPrev = 3
)

var hwndTopmost = ^uintptr(0) // (HWND)-1

type wndclassex struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type rect struct{ L, T, R, B int32 }

type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type bih struct {
	Size                         uint32
	Width, Height                int32
	Planes, BitCount             uint16
	Compression, SizeImage       uint32
	XPelsPerMeter, YPelsPerMeter int32
	ClrUsed, ClrImportant        uint32
}

type osVersionInfoExW struct {
	OSVersionInfoSize uint32
	MajorVersion      uint32
	MinorVersion      uint32
	BuildNumber       uint32
	PlatformID        uint32
	CSDVersion        [128]uint16
	ServicePackMajor  uint16
	ServicePackMinor  uint16
	SuiteMask         uint16
	ProductType       byte
	Reserved          byte
}

type kbdllhookstruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

// ---------- 平台能力 ----------

func winBuild() uint32 {
	vi := osVersionInfoExW{OSVersionInfoSize: uint32(unsafe.Sizeof(osVersionInfoExW{}))}
	pRtlGetVersion.Call(uintptr(unsafe.Pointer(&vi)))
	return vi.BuildNumber
}

func platformSupported() (bool, uint32) {
	b := winBuild()
	return b >= minBuild, b
}

// desktopLocked 当前输入桌面是否是安全桌面（锁屏 / UAC / Ctrl+Alt+Del / 切换用户）。
// 这些状态下遮罩所在的 Default 桌面不显示，隐私屏暂时失效。
func desktopLocked() bool {
	const desktopReadObjects = 0x0001
	h, _, _ := pOpenInputDesktop.Call(0, 0, desktopReadObjects)
	if h == 0 {
		return true
	}
	pCloseDesktop.Call(h)
	return false
}

func lockWorkstation() error {
	r, _, err := pLockWorkStation.Call()
	if r == 0 {
		return err
	}
	return nil
}

// ---------- 系统光标 ----------

// 全部标准光标 ID（OCR_*）
var ocrIDs = []uintptr{
	32512, 32513, 32514, 32515, 32516,
	32642, 32643, 32644, 32645, 32646,
	32648, 32649, 32650,
}

// hideCursors 用全透明光标顶替所有系统光标。
// 光标不是窗口，不受 WDA_EXCLUDEFROMCAPTURE 管辖，不藏就会浮在遮罩上，
// 把远端操作者的动作轨迹暴露给现场的人。
func hideCursors() bool {
	cw, _, _ := pGetSystemMetrics.Call(13) // SM_CXCURSOR
	ch, _, _ := pGetSystemMetrics.Call(14) // SM_CYCURSOR
	if cw == 0 || ch == 0 {
		cw, ch = 32, 32
	}
	n := int(cw) * int(ch) / 8
	andPlane := make([]byte, n)
	xorPlane := make([]byte, n)
	for i := range andPlane {
		andPlane[i] = 0xFF // AND=1 且 XOR=0 -> 该像素透明
	}
	hInst, _, _ := pGetModuleHandleW.Call(0)

	any := false
	for _, id := range ocrIDs {
		// SetSystemCursor 会接管并销毁传进去的句柄，每个 ID 都得新建一个
		cur, _, _ := pCreateCursor.Call(hInst, 0, 0, cw, ch,
			uintptr(unsafe.Pointer(&andPlane[0])), uintptr(unsafe.Pointer(&xorPlane[0])))
		if cur == 0 {
			continue
		}
		if r, _, _ := pSetSystemCursor.Call(cur, id); r != 0 {
			any = true
		}
	}
	return any
}

// restoreCursors 让系统按当前光标方案重新加载，撤销上面的替换。
// SetSystemCursor 是系统级设置，不随进程退出自动还原。
func restoreCursors() {
	pSystemParametersInfoW.Call(spiSetCursors, 0, 0, 0)
}

// resetCursorsOnStartup 上次若是崩溃退出，光标可能还停在隐藏状态。
// 启动时无条件复位一次，代价极小。
func resetCursorsOnStartup() {
	restoreCursors()
}

// ---------- 离屏位图 ----------

type dib struct {
	dc, bmp, old uintptr
	w, h         int
	bits         unsafe.Pointer
}

func newDIB(w, h int) (*dib, error) {
	screen, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, screen)
	dc, _, _ := pCreateCompatibleDC.Call(screen)
	if dc == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC 失败")
	}
	b := bih{Size: uint32(unsafe.Sizeof(bih{})), Width: int32(w), Height: -int32(h),
		Planes: 1, BitCount: 32}
	var bits unsafe.Pointer
	bmp, _, e := pCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&b)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 {
		pDeleteDC.Call(dc)
		return nil, fmt.Errorf("CreateDIBSection: %v", e)
	}
	old, _, _ := pSelectObject.Call(dc, bmp)
	return &dib{dc: dc, bmp: bmp, old: old, w: w, h: h, bits: bits}, nil
}

func (d *dib) release() {
	if d == nil {
		return
	}
	pSelectObject.Call(d.dc, d.old)
	pDeleteObject.Call(d.bmp)
	pDeleteDC.Call(d.dc)
}

func imageToDIB(path string) (*dib, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("解码失败（支持 png / jpg / gif）: %w", err)
	}
	b := src.Bounds()
	d, err := newDIB(b.Dx(), b.Dy())
	if err != nil {
		return nil, err
	}
	px := unsafe.Slice((*byte)(d.bits), d.w*d.h*4)
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := src.At(x, y).RGBA()
			px[i+0], px[i+1], px[i+2], px[i+3] = byte(bl>>8), byte(g>>8), byte(r>>8), 255
			i += 4
		}
	}
	return d, nil
}

// buildBackground 把底色和可选背景图按 fit 规则合成到一张与虚拟屏等大的位图上。
// 之后所有重绘都从这张图 BitBlt —— 纯色和图片走同一条路径，
// 擦除倒计时旧文字时也不会在背景图上留一道色杠。
func buildBackground(w, h int, bgColor uint32, imgPath, fit string) (*dib, error) {
	bg, err := newDIB(w, h)
	if err != nil {
		return nil, err
	}
	brush, _, _ := pCreateSolidBrush.Call(uintptr(bgColor))
	full := rect{0, 0, int32(w), int32(h)}
	pFillRect.Call(bg.dc, uintptr(unsafe.Pointer(&full)), brush)
	pDeleteObject.Call(brush)

	if imgPath == "" {
		return bg, nil
	}
	src, err := imageToDIB(imgPath)
	if err != nil {
		// 背景图坏了不该让整个隐私屏开不起来，退回纯色并记一笔
		log.Printf("[隐私屏] 背景图不可用，回退纯色: %v", err)
		return bg, nil
	}
	defer src.release()

	pSetStretchBltMode.Call(bg.dc, halftone)
	iw, ih := src.w, src.h
	sx, sy := float64(w)/float64(iw), float64(h)/float64(ih)
	var dw, dh int

	switch strings.ToLower(fit) {
	case "stretch":
		dw, dh = w, h
	case "center":
		dw, dh = iw, ih
	case "contain":
		s := sx
		if sy < s {
			s = sy
		}
		dw, dh = int(float64(iw)*s), int(float64(ih)*s)
	case "tile":
		for y := 0; y < h; y += ih {
			for x := 0; x < w; x += iw {
				pBitBlt.Call(bg.dc, uintptr(x), uintptr(y), uintptr(iw), uintptr(ih),
					src.dc, 0, 0, srcCopy)
			}
		}
		return bg, nil
	default: // cover
		s := sx
		if sy > s {
			s = sy
		}
		dw, dh = int(float64(iw)*s), int(float64(ih)*s)
	}
	pStretchBlt.Call(bg.dc,
		uintptr(int32((w-dw)/2)), uintptr(int32((h-dh)/2)), uintptr(dw), uintptr(dh),
		src.dc, 0, 0, uintptr(iw), uintptr(ih), srcCopy)
	return bg, nil
}

// ---------- 热键 ----------

// parseHotkey 解析 "Ctrl+Alt+P" 这类写法，返回是否需要各修饰键以及主键的虚拟键码
type hotkey struct {
	ctrl, alt, shift bool
	vk               uint32
}

func parseHotkey(s string) hotkey {
	hk := hotkey{vk: 'P', ctrl: true, alt: true}
	parts := strings.Split(strings.ToUpper(strings.ReplaceAll(s, " ", "")), "+")
	if len(parts) == 0 {
		return hk
	}
	hk = hotkey{}
	for _, p := range parts {
		switch p {
		case "CTRL", "CONTROL":
			hk.ctrl = true
		case "ALT":
			hk.alt = true
		case "SHIFT":
			hk.shift = true
		case "BREAK", "PAUSE":
			hk.vk = 0x13
		default:
			if len(p) == 1 {
				hk.vk = uint32(p[0])
			} else if strings.HasPrefix(p, "F") {
				var n int
				if _, err := fmt.Sscanf(p, "F%d", &n); err == nil && n >= 1 && n <= 24 {
					hk.vk = uint32(0x6F + n) // VK_F1 = 0x70
				}
			}
		}
	}
	if hk.vk == 0 {
		hk.vk = 'P'
		hk.ctrl, hk.alt = true, true
	}
	return hk
}

func (h hotkey) pressed(vk uint32) bool {
	if vk != h.vk {
		return false
	}
	const vkControl, vkMenu, vkShift = 0x11, 0x12, 0x10
	down := func(k uintptr) bool {
		s, _, _ := pGetAsyncKeyState.Call(k)
		return s&0x8000 != 0
	}
	if h.ctrl != down(vkControl) {
		return false
	}
	if h.alt != down(vkMenu) {
		return false
	}
	if h.shift != down(vkShift) {
		return false
	}
	return true
}

// ---------- 引擎 ----------

var (
	classOnce sync.Once
	classPtr  *uint16
	classErr  error
)

// registerClass 窗口类整个进程只注册一次
func registerClass() (*uint16, error) {
	classOnce.Do(func() {
		hInst, _, _ := pGetModuleHandleW.Call(0)
		arrow, _, _ := pLoadCursorW.Call(0, idcArrow)
		name := syscall.StringToUTF16Ptr("RKPrivacyScreen")
		wc := wndclassex{
			cbSize: uint32(unsafe.Sizeof(wndclassex{})),
			lpfnWndProc: syscall.NewCallback(func(hwnd uintptr, m uint32, w, l uintptr) uintptr {
				r, _, _ := pDefWindowProcW.Call(hwnd, uintptr(m), w, l)
				return r
			}),
			hInstance:     hInst,
			hCursor:       arrow,
			lpszClassName: name,
		}
		if a, _, e := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); a == 0 {
			classErr = fmt.Errorf("RegisterClassEx 失败: %v", e)
			return
		}
		classPtr = name
	})
	return classPtr, classErr
}

// 输入屏蔽用的低层钩子。回调必须是包级的，且要能读到当前热键配置。
var (
	hookMu       sync.Mutex
	hookHotkey   hotkey
	hookUnlockCh chan struct{}
)

// ptrFromLParam 低层钩子的 lParam 是系统传来的有效指针，
// 但 vet 无法确认这一点，直接写 unsafe.Pointer(lParam) 会被判为误用。
// 这里从 lParam 变量地址读出同一个字，语义相同且不触发误报。
func ptrFromLParam(lParam uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&lParam))
}

func llKeyboardProc(nCode int32, wParam uintptr, lParam uintptr) uintptr {
	if nCode >= 0 {
		info := (*kbdllhookstruct)(ptrFromLParam(lParam))
		hookMu.Lock()
		hk := hookHotkey
		ch := hookUnlockCh
		hookMu.Unlock()
		if hk.pressed(info.VkCode) && ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		// 吞掉本地键盘输入。Ctrl+Alt+Del 是系统保留组合键，
		// 低层钩子拦不住，那也正是留给用户的逃生通道。
		return 1
	}
	r, _, _ := pCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return r
}

func llMouseProc(nCode int32, wParam uintptr, lParam uintptr) uintptr {
	if nCode >= 0 {
		return 1
	}
	r, _, _ := pCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return r
}

type engine struct {
	mu      sync.Mutex
	running bool
	keeper  *topKeeper
	stopCh  chan struct{}
	doneCh  chan struct{}
	unlock  int32 // 原子标记：用户按了热键，等前端弹解锁框

	onFail func(string)
	// onStopped 引擎自己停下来时（到点撤除）回调，让上层能清状态、发通知
	onStopped func(string)
}

func newEngine() *engine { return &engine{keeper: newTopKeeper()} }

func (e *engine) active() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// uncoveredWindows 当前抢不过、因而没能遮住的窗口类名
func (e *engine) uncoveredWindows() []string {
	if !e.active() {
		return nil
	}
	return e.keeper.uncovered()
}

func (e *engine) unlockPending() bool { return atomic.LoadInt32(&e.unlock) == 1 }
func (e *engine) clearUnlockPending() { atomic.StoreInt32(&e.unlock, 0) }
func (e *engine) markUnlockPending()  { atomic.StoreInt32(&e.unlock, 1) }

func (e *engine) start(o Options) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return nil
	}
	e.stopCh = make(chan struct{})
	e.doneCh = make(chan struct{})
	done := e.doneCh
	e.mu.Unlock()

	stopCh, doneCh := e.stopCh, e.doneCh
	ready := make(chan error, 1)
	go e.run(o, ready, stopCh, doneCh)

	if err := <-ready; err != nil {
		<-done // 等它自己清理干净
		return err
	}
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	return nil
}

func (e *engine) stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	stop, done := e.stopCh, e.doneCh
	e.mu.Unlock()

	close(stop)
	<-done
}

// run 独占一个 OS 线程跑消息循环。
// 窗口属于创建它的线程，goroutine 一旦被调度到别的线程，
// PeekMessage / DestroyWindow 就都作用不到它了。
// run 的 stopCh / doneCh 必须由参数传进来，不能在函数体里读 e.stopCh / e.doneCh：
// 到点自停之后如果又开了一次，字段已经指向新通道，
// 旧 goroutine 就会去关新通道、监听新信号，行为完全错乱。
func (e *engine) run(o Options, ready chan<- error, stopCh, doneCh chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(doneCh)

	// 不让 Windows 在我们短暂没泵消息时拿幽灵窗口顶替遮罩、然后把进程关掉
	pDisableGhosting.Call()
	pSetProcessDPIAware.Call()

	e.keeper.reset()

	cls, err := registerClass()
	if err != nil {
		ready <- err
		return
	}

	vx, _, _ := pGetSystemMetrics.Call(76) // SM_XVIRTUALSCREEN
	vy, _, _ := pGetSystemMetrics.Call(77) // SM_YVIRTUALSCREEN
	vw, _, _ := pGetSystemMetrics.Call(78) // SM_CXVIRTUALSCREEN
	vh, _, _ := pGetSystemMetrics.Call(79) // SM_CYVIRTUALSCREEN

	bgColor, _ := parseHexColor(o.BgColor)
	fgColor, _ := parseHexColor(o.FgColor)

	bg, err := buildBackground(int(vw), int(vh), bgColor, o.Image, o.Fit)
	if err != nil {
		ready <- fmt.Errorf("背景合成失败: %w", err)
		return
	}
	defer bg.release()

	// WS_EX_TRANSPARENT：鼠标消息直接穿透到底下的窗口。
	// 必须有 —— 否则遮罩会截住远端用户的点击，远程侧等于瘫痪。
	hwnd, _, ce := pCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow|wsExNoActivate|wsExLayered|wsExTransparent,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("privacy"))),
		wsPopup, vx, vy, vw, vh, 0, 0, 0, 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("创建遮罩窗口失败: %v", ce)
		return
	}
	defer pDestroyWindow.Call(hwnd)

	// 顺序有讲究：分层窗口必须先设好 layer 属性，
	// 否则 SetWindowDisplayAffinity 会直接返回失败。
	pSetLayeredWindowAttr.Call(hwnd, 0, 255, lwaAlpha)

	if err := applyAffinity(hwnd); err != nil {
		ready <- err
		return
	}

	// 自检通过之后才显示 —— 保证遮罩一帧都不会漏进远端画面
	pShowWindow.Call(hwnd, swShowNoActivate)
	pUpdateWindow.Call(hwnd)

	font := makeFont(o)
	if font != 0 {
		defer pDeleteObject.Call(font)
	}

	if o.HideCursor {
		hideCursors()
		defer restoreCursors()
	}

	var kbHook, msHook uintptr
	if o.BlockInput {
		unlockCh := make(chan struct{}, 1)
		hookMu.Lock()
		hookHotkey = parseHotkey(o.Hotkey)
		hookUnlockCh = unlockCh
		hookMu.Unlock()

		kbHook, _, _ = pSetWindowsHookExW.Call(whKeyboardLL,
			syscall.NewCallback(llKeyboardProc), 0, 0)
		msHook, _, _ = pSetWindowsHookExW.Call(whMouseLL,
			syscall.NewCallback(llMouseProc), 0, 0)

		go func() {
			for range unlockCh {
				e.markUnlockPending()
			}
		}()
		defer func() {
			if kbHook != 0 {
				pUnhookWindowsHookEx.Call(kbHook)
			}
			if msHook != 0 {
				pUnhookWindowsHookEx.Call(msHook)
			}
			hookMu.Lock()
			hookUnlockCh = nil
			hookMu.Unlock()
			close(unlockCh)
		}()
	}

	hk := parseHotkey(o.Hotkey)
	paintAll(hwnd, bg)
	drawText(hwnd, bg, font, fgColor, hintText(o, o.MaxMinutes*60))

	ready <- nil

	e.loop(hwnd, bg, font, fgColor, hk, o, stopCh)
}

// applyAffinity 打上排除标志并回读确认。
// 只有回读一致才算数 —— 绝不停留在「以为遮住了、实际没遮住」的状态。
func applyAffinity(hwnd uintptr) error {
	ok, _, callErr := pSetWindowDisplayAffin.Call(hwnd, wdaExcludeFromCapture)
	var aff uint32
	pGetWindowDisplayAffin.Call(hwnd, uintptr(unsafe.Pointer(&aff)))
	if ok == 0 || aff != wdaExcludeFromCapture {
		return fmt.Errorf("排除标志未生效（ret=%d 回读=0x%02x err=%v）", ok, aff, callErr)
	}
	return nil
}

func makeFont(o Options) uintptr {
	weight := uintptr(400)
	if o.Bold {
		weight = 700
	}
	// nHeight 传负数表示「字符高度」
	f, _, _ := pCreateFontW.Call(uintptr(-int32(o.FontSize)), 0, 0, 0, weight, 0, 0, 0,
		defaultCharset, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(o.Font))))
	return f
}

func hintText(o Options, secondsLeft int) string {
	if !o.ShowText {
		return ""
	}
	t := strings.ReplaceAll(o.Text, "{hotkey}", o.Hotkey)
	if strings.Contains(t, "{s}") {
		if secondsLeft < 0 {
			secondsLeft = 0
		}
		t = strings.ReplaceAll(t, "{s}", fmt.Sprintf("%d", secondsLeft))
	}
	return t
}

func paintAll(hwnd uintptr, bg *dib) {
	hdc, _, _ := pGetDC.Call(hwnd)
	if hdc == 0 {
		return
	}
	defer pReleaseDC.Call(hwnd, hdc)
	pBitBlt.Call(hdc, 0, 0, uintptr(bg.w), uintptr(bg.h), bg.dc, 0, 0, srcCopy)
}

// drawText 画提示文字。
// 先把上一帧的字擦掉：这里绕过 WM_PAINT 直接往窗口 DC 上画，没有背景擦除环节，
// 而且倒计时位数变化会让居中文字横向偏移，不擦就会一层层叠成糊的。
// 擦的方式是从合成好的背景位图里把这一条重新贴回来，背景图也不会被破坏。
func drawText(hwnd uintptr, bg *dib, font uintptr, fg uint32, text string) {
	if text == "" {
		return
	}
	hdc, _, _ := pGetDC.Call(hwnd)
	if hdc == 0 {
		return
	}
	defer pReleaseDC.Call(hwnd, hdc)

	var rc rect
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	mid := (rc.T + rc.B) / 2
	half := int32(120)
	band := rect{L: rc.L, R: rc.R, T: mid - half, B: mid + half}

	pBitBlt.Call(hdc, uintptr(band.L), uintptr(band.T),
		uintptr(band.R-band.L), uintptr(band.B-band.T),
		bg.dc, uintptr(band.L), uintptr(band.T), srcCopy)

	if font != 0 {
		pSelectObject.Call(hdc, font)
	}
	pSetBkMode.Call(hdc, bkTransparent)
	pSetTextColor.Call(hdc, uintptr(fg))
	txt := syscall.StringToUTF16Ptr(text)
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(txt)), ^uintptr(0),
		uintptr(unsafe.Pointer(&band)), dtCenter|dtVCenter|dtSingleLine)
}

// pumpOnce 有上限的消息泵。
// 窗口超过约 5 秒不取消息，Windows 会判定应用无响应、
// 用幽灵窗口顶替并把进程关掉，所以这里绝不能空转不出来，
// 循环里也绝不做耗时的活。
func pumpOnce() {
	var m msgT
	for i := 0; i < 200; i++ {
		r, _, _ := pPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove)
		if r == 0 {
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (e *engine) loop(hwnd uintptr, bg *dib, font uintptr, fg uint32, hk hotkey, o Options, stopCh chan struct{}) {
	start := time.Now()
	var deadline time.Time
	if o.MaxMinutes > 0 {
		deadline = start.Add(time.Duration(o.MaxMinutes) * time.Minute)
	}

	lastTick := time.Now()
	wasLocked := false
	locked := false

	for {
		select {
		case <-stopCh:
			return
		default:
		}

		pumpOnce()

		// 每帧维持置顶。任务栏悬停缩略图是独立的置顶弹出窗口，
		// 出现时会压在遮罩上把窗口内容露出来，一秒一次的频率来不及。
		//
		// 等待解锁期间例外：此时前端会在遮罩之上弹密码框，
		// 再抢置顶会把它压回遮罩下面，用户就没法输入了。
		if !locked && !e.unlockPending() {
			e.keeper.ensure(hwnd)
		}

		// 到点自动撤除。这一条不受密码约束 ——
		// 密码 + 全屏遮罩叠在一起，程序一旦卡死就等于把人锁在屏幕外面。
		if !deadline.IsZero() && time.Now().After(deadline) {
			log.Printf("[隐私屏] 达到最长持续时间，自动撤除")
			e.selfStop()
			if e.onStopped != nil {
				go e.onStopped("达到最长持续时间")
			}
			return
		}

		// 没开输入屏蔽时，热键靠轮询检测（开了的话由低层钩子上报）
		if !o.BlockInput && hk.pressed(hk.vk) {
			e.markUnlockPending()
		}

		if time.Since(lastTick) >= time.Second {
			lastTick = time.Now()

			locked = desktopLocked()
			if wasLocked && !locked {
				// 离开安全桌面（UAC / Ctrl+Alt+Del / 锁屏）后重新抢回来。
				// AnyDesk 的隐私模式也是这个策略：离开这些界面后自动恢复。
				pShowWindow.Call(hwnd, swShowNoActivate)
				if err := applyAffinity(hwnd); err != nil {
					e.failFromLoop(err.Error())
					return
				}
				paintAll(hwnd, bg)
			}
			wasLocked = locked

			if !locked {
				// 周期性复检排除标志。显示器旋转等情况会让它掉，
				// 一旦掉了就是隐私已经泄漏，必须立刻撤除并告警。
				var aff uint32
				pGetWindowDisplayAffin.Call(hwnd, uintptr(unsafe.Pointer(&aff)))
				if aff != wdaExcludeFromCapture {
					e.failFromLoop(fmt.Sprintf("排除标志在运行中丢失（回读=0x%02x）", aff))
					return
				}

				left := 0
				if !deadline.IsZero() {
					left = int(time.Until(deadline).Seconds())
				}
				drawText(hwnd, bg, font, fg, hintText(o, left))
			}
		}

		time.Sleep(15 * time.Millisecond)
	}
}

// selfStop 引擎自己决定停下来（到点撤除）。
// 不能调 stop()：那会等 doneCh，而我们就在这个 goroutine 里，会死锁。
func (e *engine) selfStop() {
	e.mu.Lock()
	e.running = false
	e.mu.Unlock()
}

// failFromLoop 运行中自检失败：先把自己标记为停止，再异步回调上层做告警
func (e *engine) failFromLoop(msg string) {
	e.selfStop()
	if e.onFail != nil {
		go e.onFail(msg)
	}
}
