// Package security 提供密码保护闸门：托盘退出、显示主窗口等敏感操作需要先验证密码。
//
// 设计要点：
//   - 密码校验放在守护进程侧，Electron 只负责收集输入，避免渲染进程被绕过。
//   - 失败计数写入数据库，杀掉进程重启也无法把计数清零。
//   - 哈希用 Go 1.24 标准库的 crypto/pbkdf2，不引入第三方依赖。
//   - 任何失败、退出、重置动作都通过 Alerter 推到远端（飞书/钉钉/邮件）。
package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto/pbkdf2"

	"RemoteKnown/internal/storage"
)

// 配置键，统一挂在 security. 前缀下，复用现有 Config KV 表，不新增表。
const (
	keyEnabled   = "security.enabled"
	keyHash      = "security.password_hash"
	keyRecovery  = "security.recovery_hash"
	keySilent    = "security.silent_local"
	keyLockTray  = "security.lock_tray"
	keyFailCount = "security.fail_count"
	keyLockUntil = "security.lock_until"
	keyLockStage = "security.lock_stage"
)

const (
	// pbkdf2 参数：20 万次迭代在本机验证约 0.2 秒，兼顾强度与解锁手感。
	pbkdf2Iter    = 200000
	pbkdf2KeyLen  = 32
	saltLen       = 16
	maxAttempts   = 5               // 连续失败多少次触发锁定
	tokenTTL      = 5 * time.Minute // 解锁令牌有效期
	resetCodeTTL  = 5 * time.Minute // 重置验证码有效期
	minPasswordLn = 4               // 最短密码长度
)

// lockSteps 是逐级递增的锁定时长，超出后一直用最后一档。
var lockSteps = []time.Duration{5 * time.Minute, 15 * time.Minute, 60 * time.Minute}

// recoveryAlphabet 去掉了容易看错的 0/O/1/I。
const recoveryAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// Alerter 是安全事件的外发通道，由 notifier 实现。
// 用最朴素的签名，避免 security 与 notifier 之间产生类型耦合。
type Alerter interface {
	NotifySecurityEvent(title string, lines []string)
	SendResetCode(code string, ttlMinutes int) error
}

// Manager 负责密码状态、限流与解锁令牌。
type Manager struct {
	store   *storage.Storage
	alerter Alerter

	mu        sync.Mutex
	tokens    map[string]time.Time // 解锁令牌 -> 过期时间
	resetCode string
	resetExp  time.Time
	// freshRecovery 暂存"用恢复码重置后生成的新恢复码"，由前端取走展示一次。
	freshRecovery string
}

// State 是暴露给前端的安全状态，不包含任何密码材料。
type State struct {
	Enabled      bool   `json:"enabled"`
	HasPassword  bool   `json:"has_password"`
	SilentLocal  bool   `json:"silent_local"`
	LockTray     bool   `json:"lock_tray"`
	Locked       bool   `json:"locked"`
	LockedUntil  string `json:"locked_until,omitempty"`
	FailCount    int    `json:"fail_count"`
	AttemptsLeft int    `json:"attempts_left"`
	CanMailReset bool   `json:"can_mail_reset"` // 通知渠道是否可用于发验证码
}

func NewManager(store *storage.Storage, alerter Alerter) *Manager {
	return &Manager{
		store:   store,
		alerter: alerter,
		tokens:  make(map[string]time.Time),
	}
}

// ---------- 配置读写小工具 ----------

func (m *Manager) getBool(key string, def bool) bool {
	v, err := m.store.GetConfig(key)
	if err != nil || v == "" {
		return def
	}
	return v == "true"
}

func (m *Manager) setBool(key string, v bool) error {
	return m.store.SetConfig(key, strconv.FormatBool(v))
}

func (m *Manager) getInt(key string) int {
	v, err := m.store.GetConfig(key)
	if err != nil || v == "" {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

func (m *Manager) setInt(key string, v int) error {
	return m.store.SetConfig(key, strconv.Itoa(v))
}

func (m *Manager) passwordHash() string {
	v, _ := m.store.GetConfig(keyHash)
	return v
}

// lockedUntil 返回当前锁定截止时间；未锁定返回零值。
func (m *Manager) lockedUntil() time.Time {
	v, err := m.store.GetConfig(keyLockUntil)
	if err != nil || v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	if time.Now().After(t) {
		return time.Time{}
	}
	return t
}

// ---------- 对外状态 ----------

// Enabled 表示密码保护是否生效（开关打开且确实设过密码）。
func (m *Manager) Enabled() bool {
	return m.getBool(keyEnabled, false) && m.passwordHash() != ""
}

// SilentLocal 表示远程期间是否静默本地 Windows 通知。
// 只有在密码保护生效时才有意义。
func (m *Manager) SilentLocal() bool {
	return m.Enabled() && m.getBool(keySilent, true)
}

func (m *Manager) State() State {
	until := m.lockedUntil()
	fails := m.getInt(keyFailCount)
	left := maxAttempts - fails
	if left < 0 {
		left = 0
	}
	st := State{
		Enabled:      m.Enabled(),
		HasPassword:  m.passwordHash() != "",
		SilentLocal:  m.getBool(keySilent, true),
		LockTray:     m.getBool(keyLockTray, true),
		Locked:       !until.IsZero(),
		FailCount:    fails,
		AttemptsLeft: left,
		CanMailReset: m.alerter != nil,
	}
	if !until.IsZero() {
		st.LockedUntil = until.Format(time.RFC3339)
	}
	return st
}

// ---------- 哈希 ----------

func hashPassword(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成随机盐失败: %w", err)
	}
	sum, err := pbkdf2.Key(sha256.New, plain, salt, pbkdf2Iter, pbkdf2KeyLen)
	if err != nil {
		return "", fmt.Errorf("计算密码哈希失败: %w", err)
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// matchHash 用恒定时间比较，避免通过响应耗时侧信道猜测。
func matchHash(encoded, plain string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, plain, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---------- 令牌 ----------

func (m *Manager) issueToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	tok := hex.EncodeToString(buf)

	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for t, exp := range m.tokens { // 顺手清理过期令牌
		if now.After(exp) {
			delete(m.tokens, t)
		}
	}
	m.tokens[tok] = now.Add(tokenTTL)
	return tok
}

// ValidateToken 校验解锁令牌是否仍然有效。
func (m *Manager) ValidateToken(tok string) bool {
	if tok == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.tokens[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(m.tokens, tok)
		return false
	}
	return true
}

// revokeTokens 在改密码/重置/关闭保护后作废所有已发出的令牌。
func (m *Manager) revokeTokens() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = make(map[string]time.Time)
}

// ---------- 恢复码 ----------

func newRecoveryCode() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成恢复码失败: %w", err)
	}
	out := make([]byte, 0, 12)
	for _, b := range buf {
		out = append(out, recoveryAlphabet[int(b)%len(recoveryAlphabet)])
	}
	return fmt.Sprintf("RK-%s-%s-%s", out[0:4], out[4:8], out[8:12]), nil
}

// normalizeRecovery 容忍用户输入的小写、空格和缺失的连字符。
func normalizeRecovery(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.NewReplacer(" ", "", "-", "", "　", "").Replace(s)
	s = strings.TrimPrefix(s, "RK")
	if len(s) != 12 {
		return ""
	}
	return fmt.Sprintf("RK-%s-%s-%s", s[0:4], s[4:8], s[8:12])
}

// ---------- 事件外发 ----------

func (m *Manager) alert(title string, lines ...string) {
	if m.alerter == nil {
		return
	}
	go m.alerter.NotifySecurityEvent(title, lines)
}

// Alert 供包外（如心跳看门狗）复用同一条安全事件外发通道。
func (m *Manager) Alert(title string, lines ...string) {
	m.alert(title, lines...)
}

// ---------- 主要动作 ----------

// Setup 首次设置密码，返回一次性恢复码（仅此一次可见）。
func (m *Manager) Setup(password string) (string, error) {
	if m.passwordHash() != "" {
		return "", fmt.Errorf("已经设置过密码，请使用修改密码")
	}
	if len([]rune(password)) < minPasswordLn {
		return "", fmt.Errorf("密码至少 %d 位", minPasswordLn)
	}

	hash, err := hashPassword(password)
	if err != nil {
		return "", err
	}
	recovery, err := newRecoveryCode()
	if err != nil {
		return "", err
	}
	recHash, err := hashPassword(recovery)
	if err != nil {
		return "", err
	}

	if err := m.store.SetConfig(keyHash, hash); err != nil {
		return "", err
	}
	if err := m.store.SetConfig(keyRecovery, recHash); err != nil {
		return "", err
	}
	if err := m.setBool(keyEnabled, true); err != nil {
		return "", err
	}
	// 首次启用时给出默认值：托盘加锁、本地通知静默。
	if v, _ := m.store.GetConfig(keyLockTray); v == "" {
		_ = m.setBool(keyLockTray, true)
	}
	if v, _ := m.store.GetConfig(keySilent); v == "" {
		_ = m.setBool(keySilent, true)
	}
	m.resetFailures()

	log.Printf("[安全] 密码保护已启用")
	m.alert("密码保护已启用",
		"托盘菜单：需要密码",
		fmt.Sprintf("本地通知：%s", silentText(m.getBool(keySilent, true))))
	return recovery, nil
}

func silentText(silent bool) string {
	if silent {
		return "静默"
	}
	return "正常弹出"
}

// RegenerateRecovery 重新生成恢复码，旧的立即作废。需要先验证密码。
func (m *Manager) RegenerateRecovery(password string) (string, error) {
	if _, err := m.Verify(password, "重新生成恢复码"); err != nil {
		return "", err
	}
	recovery, err := newRecoveryCode()
	if err != nil {
		return "", err
	}
	recHash, err := hashPassword(recovery)
	if err != nil {
		return "", err
	}
	if err := m.store.SetConfig(keyRecovery, recHash); err != nil {
		return "", err
	}
	log.Printf("[安全] 恢复码已重新生成")
	return recovery, nil
}

// ErrLocked 表示当前处于锁定期，调用方应把 RetryAfter 展示给用户。
type ErrLocked struct {
	Until time.Time
}

func (e *ErrLocked) Error() string {
	return fmt.Sprintf("尝试次数过多，请在 %s 后重试", e.Until.Format("15:04"))
}

// ErrWrongPassword 携带剩余可试次数，方便前端提示。
type ErrWrongPassword struct {
	AttemptsLeft int
}

func (e *ErrWrongPassword) Error() string {
	if e.AttemptsLeft <= 0 {
		return "密码错误"
	}
	return fmt.Sprintf("密码错误，还剩 %d 次", e.AttemptsLeft)
}

// Verify 校验密码。成功返回解锁令牌；失败按次数限流并对外告警。
// action 用于告警文案，比如"退出程序"。
func (m *Manager) Verify(password, action string) (string, error) {
	hash := m.passwordHash()
	if hash == "" {
		return "", fmt.Errorf("尚未设置密码")
	}
	if until := m.lockedUntil(); !until.IsZero() {
		return "", &ErrLocked{Until: until}
	}

	if matchHash(hash, password) {
		m.resetFailures()
		return m.issueToken(), nil
	}

	fails := m.getInt(keyFailCount) + 1
	_ = m.setInt(keyFailCount, fails)

	if action == "" {
		action = "验证密码"
	}

	if fails >= maxAttempts {
		stage := m.getInt(keyLockStage)
		if stage >= len(lockSteps) {
			stage = len(lockSteps) - 1
		}
		until := time.Now().Add(lockSteps[stage])
		_ = m.store.SetConfig(keyLockUntil, until.Format(time.RFC3339))
		_ = m.setInt(keyLockStage, stage+1)
		_ = m.setInt(keyFailCount, 0)

		log.Printf("[安全] 连续失败 %d 次，锁定至 %s", fails, until.Format("15:04:05"))
		m.alert("连续错误已锁定",
			fmt.Sprintf("事件：密码连续错误 %d 次", maxAttempts),
			fmt.Sprintf("处理：解锁入口锁定至 %s", until.Format("15:04")),
			"监控：未中断，仍在运行")
		return "", &ErrLocked{Until: until}
	}

	left := maxAttempts - fails
	log.Printf("[安全] 密码错误（%d/%d），操作：%s", fails, maxAttempts, action)
	m.alert("操作被拦截",
		fmt.Sprintf("事件：有人尝试%s，密码错误 (%d/%d)", action, fails, maxAttempts),
		"状态：程序仍在运行")
	return "", &ErrWrongPassword{AttemptsLeft: left}
}

func (m *Manager) resetFailures() {
	_ = m.setInt(keyFailCount, 0)
	_ = m.setInt(keyLockStage, 0)
	_ = m.store.SetConfig(keyLockUntil, "")
}

// Change 修改密码。
func (m *Manager) Change(oldPassword, newPassword string) error {
	if _, err := m.Verify(oldPassword, "修改密码"); err != nil {
		return err
	}
	if len([]rune(newPassword)) < minPasswordLn {
		return fmt.Errorf("密码至少 %d 位", minPasswordLn)
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := m.store.SetConfig(keyHash, hash); err != nil {
		return err
	}
	m.revokeTokens()
	log.Printf("[安全] 密码已修改")
	m.alert("保护密码已修改", "提示：若不是你本人操作，请立即检查这台机器")
	return nil
}

// Disable 关闭密码保护，需要先验证当前密码。
func (m *Manager) Disable(password string) error {
	if _, err := m.Verify(password, "关闭密码保护"); err != nil {
		return err
	}
	if err := m.setBool(keyEnabled, false); err != nil {
		return err
	}
	if err := m.store.SetConfig(keyHash, ""); err != nil {
		return err
	}
	if err := m.store.SetConfig(keyRecovery, ""); err != nil {
		return err
	}
	m.revokeTokens()
	m.resetFailures()
	log.Printf("[安全] 密码保护已关闭")
	m.alert("密码保护已关闭", "提示：现在任何人都可以直接退出本程序")
	return nil
}

// SetOptions 更新静默与托盘开关，nil 表示保持不变。
func (m *Manager) SetOptions(silentLocal, lockTray *bool) error {
	if silentLocal != nil {
		if err := m.setBool(keySilent, *silentLocal); err != nil {
			return err
		}
	}
	if lockTray != nil {
		if err := m.setBool(keyLockTray, *lockTray); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 重置 ----------

// RequestReset 生成 6 位验证码并通过已配置的通知渠道发出。
// 这条请求本身就是一次告警：真正的机主没点，就说明有人在尝试接管。
func (m *Manager) RequestReset() error {
	if m.passwordHash() == "" {
		return fmt.Errorf("尚未设置密码")
	}
	if m.alerter == nil {
		return fmt.Errorf("通知渠道不可用，请改用恢复码")
	}

	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("生成验证码失败: %w", err)
	}
	code := fmt.Sprintf("%06d", (int(buf[0])<<16|int(buf[1])<<8|int(buf[2]))%1000000)

	m.mu.Lock()
	m.resetCode = code
	m.resetExp = time.Now().Add(resetCodeTTL)
	m.mu.Unlock()

	if err := m.alerter.SendResetCode(code, int(resetCodeTTL/time.Minute)); err != nil {
		return fmt.Errorf("验证码发送失败: %w", err)
	}
	log.Printf("[安全] 重置验证码已通过通知渠道发出")
	return nil
}

// ConfirmReset 用验证码或恢复码重置密码，两者给其一即可。
func (m *Manager) ConfirmReset(code, recovery, newPassword string) error {
	if len([]rune(newPassword)) < minPasswordLn {
		return fmt.Errorf("密码至少 %d 位", minPasswordLn)
	}

	via := ""
	switch {
	case strings.TrimSpace(code) != "":
		m.mu.Lock()
		want, exp := m.resetCode, m.resetExp
		m.mu.Unlock()
		if want == "" || time.Now().After(exp) {
			return fmt.Errorf("验证码已过期，请重新获取")
		}
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(code)), []byte(want)) != 1 {
			return fmt.Errorf("验证码不正确")
		}
		via = "通知渠道验证码"

	case strings.TrimSpace(recovery) != "":
		norm := normalizeRecovery(recovery)
		recHash, _ := m.store.GetConfig(keyRecovery)
		if norm == "" || recHash == "" || !matchHash(recHash, norm) {
			return fmt.Errorf("恢复码不正确")
		}
		via = "恢复码"

	default:
		return fmt.Errorf("请提供验证码或恢复码")
	}

	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := m.store.SetConfig(keyHash, hash); err != nil {
		return err
	}

	// 恢复码是一次性的，用掉之后换一张新的并让机主重新保存。
	if via == "恢复码" {
		if fresh, err := newRecoveryCode(); err == nil {
			if h, err := hashPassword(fresh); err == nil {
				_ = m.store.SetConfig(keyRecovery, h)
				m.mu.Lock()
				m.freshRecovery = fresh
				m.mu.Unlock()
			}
		}
	}

	m.mu.Lock()
	m.resetCode, m.resetExp = "", time.Time{}
	m.mu.Unlock()

	m.revokeTokens()
	m.resetFailures()

	log.Printf("[安全] 密码已通过%s重置", via)
	m.alert("保护密码已重置",
		fmt.Sprintf("方式：%s", via),
		"提示：若不是你操作，请立即检查这台机器")
	return nil
}

// TakeFreshRecovery 取出一次性的新恢复码（用恢复码重置后生成），取过即清空。
func (m *Manager) TakeFreshRecovery() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.freshRecovery
	m.freshRecovery = ""
	return v
}
