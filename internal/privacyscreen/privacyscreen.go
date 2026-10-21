// Package privacyscreen 隐私屏：远程会话期间遮住本机物理屏幕，而远端画面不受影响。
//
// 原理是给一个覆盖整个虚拟屏幕的置顶窗口打上 WDA_EXCLUDEFROMCAPTURE，
// DWM 合成时只把它送往物理显示器、不送往任何抓屏管线。
// 原理验证与踩坑记录见 tech_doc/toolcmd/privacydemo。
package privacyscreen

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"RemoteKnown/internal/storage"

	"github.com/google/uuid"
)

// 触发方式
const (
	TriggerOff    = "off"    // 关闭，功能完全不介入（默认）
	TriggerManual = "manual" // 只能手动开启
	TriggerAuto   = "auto"   // 检测到远程会话时自动开启
)

// 配置项在 KV 表里的键名
const (
	keyAutoCountdown = "privacy_auto_countdown"
	keyTrigger       = "privacy_trigger"
	keyBgColor       = "privacy_bg_color"
	keyImage         = "privacy_image"
	keyImagePreset   = "privacy_image_preset"
	keyFit           = "privacy_fit"
	keyShowText      = "privacy_show_text"
	keyText          = "privacy_text"
	keyFont          = "privacy_font"
	keyFontSize      = "privacy_font_size"
	keyBold          = "privacy_bold"
	keyFgColor       = "privacy_fg_color"
	keyHideCursor    = "privacy_hide_cursor"
	keyBlockInput    = "privacy_block_input"
	keyRequirePw     = "privacy_require_password"
	keyHotkey        = "privacy_hotkey"
	keyMaxMinutes    = "privacy_max_minutes"
	keyDropOnEnd     = "privacy_drop_on_end"
	keyLockOnEnd     = "privacy_lock_on_end"
)

// Options 隐私屏的全部可配置项。
// 外观部分与 tech_doc/toolcmd/privacydemo 的命令行选项一一对应。
type Options struct {
	Trigger       string `json:"trigger"`        // off / manual / auto
	AutoCountdown bool   `json:"auto_countdown"` // 自动开启前显示 10 秒倒计时，默认关闭

	// 外观
	BgColor     string `json:"bg_color"`     // #RRGGBB
	Image       string `json:"image"`        // 用户自选背景图路径，空则用内置图或纯色
	ImagePreset string `json:"image_preset"` // 内置背景图标识，空表示不用内置图
	Fit         string `json:"fit"`          // cover / contain / stretch / center / tile
	ShowText    bool   `json:"show_text"`    //
	Text        string `json:"text"`         // {s} 替换成剩余秒数
	Font        string `json:"font"`         //
	FontSize    int    `json:"font_size"`    // 像素高
	Bold        bool   `json:"bold"`         //
	FgColor     string `json:"fg_color"`     // #RRGGBB

	// 行为
	HideCursor bool `json:"hide_cursor"` // 光标不受窗口合成管辖，不藏会暴露远端操作轨迹
	BlockInput bool `json:"block_input"` // 屏蔽本地键鼠，防止旁人打断远端操作

	// 退出
	RequirePassword bool   `json:"require_password"` // 撤除需要密码（复用「安全」页那套）
	Hotkey          string `json:"hotkey"`           // 呼出解锁的全局热键
	MaxMinutes      int    `json:"max_minutes"`      // 最长持续，0 = 不限制
	DropOnEnd       bool   `json:"drop_on_end"`      // 远程结束时自动撤除
	LockOnEnd       bool   `json:"lock_on_end"`      // 远程结束后锁定 Windows
}

// DefaultOptions 出厂默认值：触发方式为关闭，功能不介入。
func DefaultOptions() Options {
	return Options{
		Trigger:         TriggerOff,
		AutoCountdown:   false,
		BgColor:         "#0A2A43",
		ImagePreset:     "aurora",
		Fit:             "cover",
		ShowText:        true,
		Text:            "本机已开启隐私屏 · 按 {hotkey} 解锁",
		Font:            "Microsoft YaHei",
		FontSize:        12,
		Bold:            true,
		FgColor:         "#FFFFFF",
		HideCursor:      true,
		BlockInput:      false,
		RequirePassword: true,
		Hotkey:          "Ctrl+Alt+P",
		MaxMinutes:      30,
		DropOnEnd:       true,
		LockOnEnd:       false,
	}
}

// Status 给前端看的运行状态
type Status struct {
	Supported     bool   `json:"supported"`      // 系统是否支持 WDA_EXCLUDEFROMCAPTURE
	Build         uint32 `json:"build"`          // Windows build 号
	MinBuild      uint32 `json:"min_build"`      // 需要的最低 build
	Reason        string `json:"reason"`         // 不支持时的原因
	Active        bool   `json:"active"`         // 遮罩是否正生效
	Trigger       string `json:"trigger"`        //
	DesktopLocked bool   `json:"desktop_locked"` // 当前是否处于锁屏/安全桌面
	StartedAt     string `json:"started_at"`     //
	UnlockPending bool   `json:"unlock_pending"` // 用户按了热键，等待前端弹解锁框
	LastError     string `json:"last_error"`     // 最近一次失败原因
	AutoPending   bool   `json:"auto_pending"`   // 自动开启前等待用户取消
	AutoRequestID string `json:"auto_request_id"`
	AutoDeadline  string `json:"auto_deadline"` // RFC3339Nano

	// Uncovered 遮罩盖不住的窗口类名。典型是任务管理器这类 UIAccess 提权窗口，
	// 普通权限进程抢不到它上面去。这些窗口在遮罩期间仍然可见，属于没遮住，
	// 必须如实告诉用户，不能假装全遮住了。
	Uncovered []string `json:"uncovered"`
}

// Alerter 失败告警出口。用 internal/notifier 实现，
// 走飞书 / 钉钉 / 邮件 —— 遮罩生效时本地弹窗会被自己盖住，必须有远程渠道。
type Alerter interface {
	NotifySecurityEvent(title string, lines []string)
}

const autoPromptDelay = 10 * time.Second

// ErrAutoRequestExpired 表示请求已处理、过期或属于旧会话。
var ErrAutoRequestExpired = errors.New("自动隐私屏请求已过期或失效")

// maskEngine 让状态机可在测试中使用空引擎，避免真正遮住屏幕。
type maskEngine interface {
	active() bool
	unlockPending() bool
	uncoveredWindows() []string
	clearUnlockPending()
	start(Options) error
	stop()
}

// Manager 隐私屏的对外入口
type Manager struct {
	store *storage.Storage
	alert Alerter

	mu        sync.Mutex
	eng       maskEngine
	startedAt time.Time
	lastErr   string

	remoteActive  bool
	shutdown      bool
	autoRequestID string
	autoDeadline  time.Time
	autoShown     bool
	autoTimer     *time.Timer
}

func NewManager(store *storage.Storage, alert Alerter) *Manager {
	m := &Manager{store: store, alert: alert}
	eng := newEngine()
	eng.onFail = m.handleEngineFailure
	eng.onStopped = m.handleEngineStopped
	m.eng = eng
	// 上次若是崩溃退出，系统光标可能还停在隐藏状态 —— 无条件复位一次
	resetCursorsOnStartup()
	return m
}

// ---------- 配置读写 ----------

func (m *Manager) getStr(key, def string) string {
	v, err := m.store.GetConfig(key)
	if err != nil || v == "" {
		return def
	}
	return v
}

func (m *Manager) getBool(key string, def bool) bool {
	v, err := m.store.GetConfig(key)
	if err != nil || v == "" {
		return def
	}
	return v == "true" || v == "1"
}

func (m *Manager) getInt(key string, def int) int {
	v, err := m.store.GetConfig(key)
	if err != nil || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Options 读取当前配置，缺失项用默认值补齐
func (m *Manager) Options() Options {
	d := DefaultOptions()
	o := Options{
		Trigger:         m.getStr(keyTrigger, d.Trigger),
		AutoCountdown:   m.getBool(keyAutoCountdown, d.AutoCountdown),
		BgColor:         m.getStr(keyBgColor, d.BgColor),
		Image:           m.getStr(keyImage, d.Image),
		ImagePreset:     m.getStr(keyImagePreset, d.ImagePreset),
		Fit:             m.getStr(keyFit, d.Fit),
		ShowText:        m.getBool(keyShowText, d.ShowText),
		Text:            m.getStr(keyText, d.Text),
		Font:            m.getStr(keyFont, d.Font),
		FontSize:        m.getInt(keyFontSize, d.FontSize),
		Bold:            m.getBool(keyBold, d.Bold),
		FgColor:         m.getStr(keyFgColor, d.FgColor),
		HideCursor:      m.getBool(keyHideCursor, d.HideCursor),
		BlockInput:      m.getBool(keyBlockInput, d.BlockInput),
		RequirePassword: m.getBool(keyRequirePw, d.RequirePassword),
		Hotkey:          m.getStr(keyHotkey, d.Hotkey),
		MaxMinutes:      m.getInt(keyMaxMinutes, d.MaxMinutes),
		DropOnEnd:       m.getBool(keyDropOnEnd, d.DropOnEnd),
		LockOnEnd:       m.getBool(keyLockOnEnd, d.LockOnEnd),
	}
	// 存进去的互斥关系读出来再补一遍：选了自定义图片时 preset 落库是空串，
	// 而 getStr 把空串当缺失回默认，不拦一下自定义图片就永远甩不掉内置图。
	if o.Image != "" {
		o.ImagePreset = ""
	}
	return o
}

// SaveOptions 写入配置，写前做一遍取值校正
func (m *Manager) SaveOptions(o Options) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o = sanitize(o)
	pairs := []struct {
		k, v string
	}{
		{keyTrigger, o.Trigger},
		{keyAutoCountdown, boolStr(o.AutoCountdown)},
		{keyBgColor, o.BgColor},
		{keyImage, o.Image},
		{keyImagePreset, o.ImagePreset},
		{keyFit, o.Fit},
		{keyShowText, boolStr(o.ShowText)},
		{keyText, o.Text},
		{keyFont, o.Font},
		{keyFontSize, strconv.Itoa(o.FontSize)},
		{keyBold, boolStr(o.Bold)},
		{keyFgColor, o.FgColor},
		{keyHideCursor, boolStr(o.HideCursor)},
		{keyBlockInput, boolStr(o.BlockInput)},
		{keyRequirePw, boolStr(o.RequirePassword)},
		{keyHotkey, o.Hotkey},
		{keyMaxMinutes, strconv.Itoa(o.MaxMinutes)},
		{keyDropOnEnd, boolStr(o.DropOnEnd)},
		{keyLockOnEnd, boolStr(o.LockOnEnd)},
	}
	for _, p := range pairs {
		if err := m.store.SetConfig(p.k, p.v); err != nil {
			return err
		}
		if (p.k == keyTrigger && o.Trigger != TriggerAuto) ||
			(p.k == keyAutoCountdown && !o.AutoCountdown) {
			m.cancelAutoLocked()
		}
	}
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func sanitize(o Options) Options {
	switch o.Trigger {
	case TriggerManual, TriggerAuto:
	default:
		o.Trigger = TriggerOff
	}
	switch strings.ToLower(o.Fit) {
	case "cover", "contain", "stretch", "center", "tile":
		o.Fit = strings.ToLower(o.Fit)
	default:
		o.Fit = "cover"
	}
	if _, err := parseHexColor(o.BgColor); err != nil {
		o.BgColor = "#0A2A43"
	}
	if _, err := parseHexColor(o.FgColor); err != nil {
		o.FgColor = "#FFFFFF"
	}
	if o.FontSize < 8 || o.FontSize > 400 {
		o.FontSize = 12
	}
	if o.Font == "" {
		o.Font = "Microsoft YaHei"
	}
	if o.MaxMinutes < 0 || o.MaxMinutes > 24*60 {
		o.MaxMinutes = 30
	}
	if o.Hotkey == "" {
		o.Hotkey = "Ctrl+Alt+P"
	}
	// 选了自己指定的图片就不再看内置图，两者互斥。
	// "none" 是用户明确选的纯色，保留；其余非法值回退默认内置图。
	if o.Image != "" {
		o.ImagePreset = ""
	} else if o.ImagePreset != "none" && !IsBuiltinPreset(o.ImagePreset) {
		o.ImagePreset = DefaultOptions().ImagePreset
	}
	return o
}

// parseHexColor 把 #RRGGBB 解析成 Win32 的 COLORREF（0x00BBGGRR，字节序相反）
func parseHexColor(s string) (uint32, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return 0, fmt.Errorf("颜色格式应为 #RRGGBB，收到 %q", s)
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("颜色格式应为 #RRGGBB，收到 %q", s)
	}
	r, g, b := (v>>16)&0xFF, (v>>8)&0xFF, v&0xFF
	return uint32(b<<16 | g<<8 | r), nil
}

// ---------- 运行 ----------

// notify 统一的通知出口。
//
// NotifySecurityEvent 是同步的（要走 HTTP / SMTP），绝不能在持有 m.mu 时直接调 ——
// 那会让 /api/privacy/status 这类请求跟着一起卡住。
// 所以除进程退出那一次之外，一律异步发出去。
func (m *Manager) notify(title string, lines []string, waitForIt bool) {
	if m.alert == nil {
		return
	}
	if waitForIt {
		// 进程马上就要结束了，异步发根本来不及送出去
		m.alert.NotifySecurityEvent(title, lines)
		return
	}
	go m.alert.NotifySecurityEvent(title, lines)
}

// describe 把当前遮罩配置写成人能读的几行，附在开启通知里，
// 方便机主一眼确认「现在那块屏幕上到底盖的是什么」。
func describe(o Options) []string {
	look := "纯色 " + o.BgColor
	if o.Image != "" {
		look = fmt.Sprintf("背景图 %s（%s）", o.Image, o.Fit)
	}
	exit := "热键 " + o.Hotkey
	if o.RequirePassword {
		exit += "（需要密码）"
	}
	limit := "不限制"
	if o.MaxMinutes > 0 {
		limit = fmt.Sprintf("%d 分钟", o.MaxMinutes)
	}
	lines := []string{
		"遮罩：" + look,
		"退出：" + exit,
		"最长持续：" + limit,
	}
	if o.HideCursor {
		lines = append(lines, "已隐藏鼠标光标")
	}
	if o.BlockInput {
		lines = append(lines, "已屏蔽本地键鼠输入")
	}
	return lines
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
}

// Status 当前状态
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	ok, build := platformSupported()
	st := Status{
		Supported:     ok,
		Build:         build,
		MinBuild:      minBuild,
		Active:        m.eng.active(),
		Trigger:       m.Options().Trigger,
		DesktopLocked: desktopLocked(),
		UnlockPending: m.eng.unlockPending(),
		Uncovered:     m.eng.uncoveredWindows(),
		LastError:     m.lastErr,
	}
	if !ok {
		st.Reason = fmt.Sprintf(
			"需要 Windows 10 2004（build %d）及以上；当前 build %d。"+
				"更低版本会把排除标志降级成 WDA_MONITOR，导致远端画面也变黑，因此直接禁用。",
			minBuild, build)
	}
	if !m.startedAt.IsZero() {
		st.StartedAt = m.startedAt.Format(time.RFC3339)
	}
	if m.autoRequestID != "" {
		st.AutoPending = true
		st.AutoRequestID = m.autoRequestID
		st.AutoDeadline = m.autoDeadline.Format(time.RFC3339Nano)
	}
	return st
}

// Enable 开启遮罩。reason 用于日志与告警文案。
//
// 关键约定：先做自检，确认排除标志真的生效了才显示窗口 ——
// 保证遮罩一帧都不会漏进远端画面；自检不过就直接失败并告警，
// 绝不停留在「以为遮住了、实际没遮住」的状态。
func (m *Manager) Enable(reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelAutoLocked()
	return m.enableLocked(reason)
}

func (m *Manager) enableLocked(reason string) error {
	if m.shutdown {
		return errors.New("隐私屏模块已退出")
	}
	if m.eng.active() {
		return nil
	}
	if ok, build := platformSupported(); !ok {
		err := fmt.Errorf("系统不支持：需要 build %d 及以上，当前 %d", minBuild, build)
		m.fail("隐私屏无法开启", err, reason)
		return err
	}
	if desktopLocked() {
		err := fmt.Errorf("当前处于锁屏或安全桌面，遮罩无法显示")
		m.fail("隐私屏无法开启", err, reason)
		return err
	}

	o := sanitize(m.Options())
	if err := m.eng.start(o); err != nil {
		m.fail("隐私屏未能生效", err, reason)
		return err
	}

	m.startedAt = time.Now()
	m.lastErr = ""
	log.Printf("[隐私屏] 已开启（%s）", reason)
	m.notify("隐私屏已开启", append([]string{"触发：" + reason}, describe(o)...), false)
	return nil
}

// Disable 撤除遮罩。密码校验由调用方（HTTP handler）在此之前完成。
func (m *Manager) Disable(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disableLocked(reason, false)
}

// disableLocked 撤除遮罩并通知。waitForNotify 只在进程退出时为真。
func (m *Manager) disableLocked(reason string, waitForNotify bool) {
	m.cancelAutoLocked()
	if !m.eng.active() {
		return
	}
	lasted := time.Duration(0)
	if !m.startedAt.IsZero() {
		lasted = time.Since(m.startedAt)
	}
	m.eng.stop()
	m.startedAt = time.Time{}
	log.Printf("[隐私屏] 已撤除（%s）", reason)
	m.notify("隐私屏已撤除", []string{
		"原因：" + reason,
		"本次持续：" + humanDuration(lasted),
		"这块屏幕现在恢复正常显示了。",
	}, waitForNotify)
}

// fail 统一的失败出口：记录 + 告警。
// 遮罩没成功就意味着隐私已经泄漏，这个通知不可省略，
// 且必须走远程渠道 —— 遮罩生效时本地弹窗会被自己盖住。
func (m *Manager) fail(title string, err error, reason string) {
	m.lastErr = err.Error()
	log.Printf("[隐私屏] %s：%v（%s）", title, err, reason)
	m.notify(title, []string{
		"原因：" + err.Error(),
		"触发：" + reason,
		"时间：" + time.Now().Format("2006-01-02 15:04:05"),
		"屏幕内容在此期间可能已被现场人员看到。",
	}, false)
}

// handleEngineFailure 遮罩中途失效（比如显示器旋转后排除标志掉了）时由引擎回调
func (m *Manager) handleEngineFailure(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 引擎在自己的 goroutine 里已经停下了，这里不能再调 stop()
	m.startedAt = time.Time{}
	m.fail("隐私屏中途失效", errors.New(msg), "运行中自检")
}

// handleEngineStopped 引擎自己停下来了（到点撤除）。
// 它已经在自己的 goroutine 里收拾干净了，这里只需清状态并通知。
func (m *Manager) handleEngineStopped(reason string) {
	m.mu.Lock()
	lasted := time.Duration(0)
	if !m.startedAt.IsZero() {
		lasted = time.Since(m.startedAt)
	}
	m.startedAt = time.Time{}
	m.mu.Unlock()

	log.Printf("[隐私屏] 已撤除（%s）", reason)
	m.notify("隐私屏已撤除", []string{
		"原因：" + reason,
		"本次持续：" + humanDuration(lasted),
		"这块屏幕现在恢复正常显示了。",
	}, false)
}

// ClearUnlockPending 前端已经把解锁框弹出来了，清掉标记
func (m *Manager) ClearUnlockPending() {
	m.eng.clearUnlockPending()
}

// ---------- 与检测器联动 ----------

// cancelAutoLocked 清除请求，同时让已进入调度队列的旧回调失效。
func (m *Manager) cancelAutoLocked() {
	if m.autoTimer != nil {
		m.autoTimer.Stop()
		m.autoTimer = nil
	}
	m.autoRequestID = ""
	m.autoDeadline = time.Time{}
	m.autoShown = false
}

func (m *Manager) scheduleAutoLocked() {
	if m.autoTimer != nil {
		m.autoTimer.Stop()
	}
	id, deadline := m.autoRequestID, m.autoDeadline
	m.autoTimer = time.AfterFunc(time.Until(deadline), func() {
		m.finishAuto(id, deadline)
	})
}

func (m *Manager) finishAuto(id string, deadline time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// shown 会保留 ID 并更新 deadline，因此两者都要核对。
	if m.shutdown || id == "" || m.autoRequestID != id || !m.autoDeadline.Equal(deadline) {
		return
	}
	if time.Now().Before(deadline) {
		m.scheduleAutoLocked()
		return
	}
	m.cancelAutoLocked()
	o := m.Options()
	if !m.remoteActive || o.Trigger != TriggerAuto || !o.AutoCountdown {
		return
	}
	if err := m.enableLocked("检测到远程会话"); err != nil {
		log.Printf("[隐私屏] 自动开启失败: %v", err)
	}
}

// AutoAction 仅处理当前未过期的提示请求，不涉及已生效遮罩的密码保护。
// shown 只在窗口首次 ready 时续一次完整倒计时，重复上报不会继续延期。
func (m *Manager) AutoAction(requestID, action string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown || requestID == "" || requestID != m.autoRequestID ||
		!time.Now().Before(m.autoDeadline) {
		return ErrAutoRequestExpired
	}
	switch action {
	case "shown":
		if !m.autoShown {
			m.autoShown = true
			m.autoDeadline = time.Now().Add(autoPromptDelay)
			m.scheduleAutoLocked()
		}
	case "cancel":
		m.cancelAutoLocked()
		log.Printf("[隐私屏] 用户取消本次自动开启")
		m.notify("本次隐私屏自动开启已取消", []string{
			"操作：用户在倒计时提示中取消了本次隐私屏自动开启。",
			"结果：本次远程会话不再自动开启隐私屏，本机屏幕仍可见。",
			"远程检测、会话记录和远程会话通知继续正常运行。",
		}, false)
	default:
		return errors.New("自动隐私屏操作必须为 shown 或 cancel")
	}
	return nil
}

// OnRemoteStart 同一会话只创建一次请求，取消后不会在轮询中重复提示。
func (m *Manager) OnRemoteStart() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown || m.remoteActive {
		return
	}
	m.remoteActive = true
	o := m.Options()
	if o.Trigger != TriggerAuto || m.eng.active() {
		return
	}
	if !o.AutoCountdown {
		if err := m.enableLocked("检测到远程会话"); err != nil {
			log.Printf("[隐私屏] 自动开启失败: %v", err)
		}
		return
	}
	m.autoRequestID = uuid.NewString()
	m.autoDeadline = time.Now().Add(autoPromptDelay)
	m.autoShown = false
	m.scheduleAutoLocked()
}

// OnRemoteEnd 远程会话结束：按配置撤除遮罩、并可选地锁定 Windows
func (m *Manager) OnRemoteEnd() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remoteActive = false
	m.cancelAutoLocked()
	if m.shutdown {
		return
	}
	o := m.Options()
	if o.DropOnEnd {
		// 没人在操作了，遮罩不该继续盖着；这一步不要求密码
		m.disableLocked("远程会话结束", false)
	}
	if o.LockOnEnd {
		if err := lockWorkstation(); err != nil {
			log.Printf("[隐私屏] 远程结束后锁屏失败: %v", err)
		} else {
			log.Printf("[隐私屏] 远程结束后已锁定 Windows")
		}
	}
}

// Shutdown 进程退出前清理，确保不把光标和输入屏蔽留在异常状态
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdown = true
	m.disableLocked("程序退出", true)
}
