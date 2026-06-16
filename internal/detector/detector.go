package detector

import (
	"RemoteKnown/internal/storage"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

var debugMode = os.Getenv("REMOTEKNOWN_DEBUG") != ""

type Signal struct {
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Confidence float64   `json:"confidence"`
	Source     string    `json:"source"`
	DetectedAt time.Time `json:"detected_at"`
}

type DetectionResult struct {
	RemoteActive bool      `json:"remote_active"`
	StartTime    time.Time `json:"start_time,omitempty"`
	Duration     string    `json:"duration,omitempty"`
	Signals      []Signal  `json:"signals"`
	OverallConf  float64   `json:"overall_confidence"`
}

type Detector struct {
	storage     *storage.Storage
	notifier    Notifier
	windows     *WindowsDetector
	signals     []Signal
	lastState   bool
	lastChange  time.Time
	stateMutex  sync.RWMutex
	signalMutex sync.RWMutex
	windowsMu   sync.Mutex
}

// Notifier 接口，避免循环依赖
type Notifier interface {
	NotifyRemoteStart(signals []NotifierSignal)
	NotifyRemoteEnd(signals []NotifierSignal)
}

// NotifierSignal 接口，用于通知
type NotifierSignal interface {
	GetName() string
}

// notifierSignal 实现 NotifierSignal 接口
type notifierSignal struct {
	name string
}

func (n notifierSignal) GetName() string {
	return n.name
}

// 简化：不再使用复杂的置信度系统
const (
	ConfRemoteTool float64 = 1.0 // 检测到远程工具就是1.0
)

func NewDetector(storage *storage.Storage, notifier Notifier) *Detector {
	d := &Detector{
		storage:    storage,
		notifier:   notifier,
		windows:    NewWindowsDetector(),
		lastState:  false,
		lastChange: time.Now(),
	}
	// 启动检测循环前，先从 SQLite 载入检测规则（首次运行写入内置默认规则作为种子）
	if err := d.loadRules(); err != nil {
		log.Printf("[检测器] 载入检测规则失败: %v", err)
	}
	go d.detectionLoop()
	return d
}

// loadRules 从 SQLite 载入当前生效的检测规则并注入 windows 检测器。
// 若数据库中尚无任何规则集（首次运行），则把内置默认规则写入 SQLite 作为种子并置为生效。
func (d *Detector) loadRules() error {
	has, err := d.storage.HasAnyRuleSet()
	if err != nil {
		return err
	}
	if !has {
		// 首次运行：种子内置默认规则
		rulesJSON, err := DefaultRulesJSON()
		if err != nil {
			return err
		}
		ruleSet, err := d.storage.SaveRuleSet(DefaultRulesVersion, DefaultRulesVersion, rulesJSON, "builtin")
		if err != nil {
			return err
		}
		if err := d.storage.SetActiveRuleSet(ruleSet.ID); err != nil {
			return err
		}
		log.Printf("[检测器] 首次初始化内置检测规则 v%s", DefaultRulesVersion)
	}
	return d.applyActiveRules()
}

// applyActiveRules 读取当前生效规则集并注入 windows 检测器。
func (d *Detector) applyActiveRules() error {
	active, err := d.storage.GetActiveRuleSet()
	if err != nil {
		return err
	}
	if active == nil {
		return fmt.Errorf("无生效的检测规则")
	}
	rules, err := ParseRules(active.Rules)
	if err != nil {
		return fmt.Errorf("解析规则 JSON 失败: %w", err)
	}
	d.windowsMu.Lock()
	d.windows.SetRules(rules)
	d.windowsMu.Unlock()
	log.Printf("[检测器] 已载入检测规则 v%s（%d 条，来源:%s）", active.Version, len(rules), active.Source)
	return nil
}

// ReloadRules 重新载入当前生效的检测规则（应用/回滚规则后调用，热更新）。
func (d *Detector) ReloadRules() error {
	return d.applyActiveRules()
}

// GetActiveRuleVersion 返回当前生效规则集的版本号。
func (d *Detector) GetActiveRuleVersion() (string, error) {
	active, err := d.storage.GetActiveRuleSet()
	if err != nil {
		return "", err
	}
	if active == nil {
		return "", nil
	}
	return active.Version, nil
}

// ListRuleVersions 返回所有规则集版本（供 UI 展示与回滚）。
func (d *Detector) ListRuleVersions() ([]storage.DetectionRuleSet, error) {
	return d.storage.ListRuleSets()
}

func (d *Detector) detectionLoop() {
	// 优化：将检测间隔从2秒增加到5秒，减少CPU占用
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		d.detect()
	}
}

func (d *Detector) detect() {
	var allSignals []Signal

	d.windowsMu.Lock()
	// 使用新的简化检测方法：检测远程工具（进程+窗口类名）
	remoteToolSignals, _ := d.windows.DetectRemoteTools()
	// 其他检测方法保留作为占位符
	sessionSignals, _ := d.windows.DetectSessions()
	portSignals, _ := d.windows.DetectRDPPorts()
	d.windowsMu.Unlock()

	allSignals = append(allSignals, remoteToolSignals...)
	allSignals = append(allSignals, sessionSignals...)
	allSignals = append(allSignals, portSignals...)

	d.stateMutex.Lock()
	d.signals = allSignals

	isRemote := len(remoteToolSignals) > 0 || len(sessionSignals) > 0

	if isRemote != d.lastState {
		d.lastChange = time.Now()
		d.lastState = isRemote

		if isRemote {
			d.handleRemoteStart(allSignals)
		} else {
			d.handleRemoteEnd()
		}
	}

	d.stateMutex.Unlock()

	if debugMode {
		log.Printf("检测结果: 远程=%v, 信号数=%d", isRemote, len(allSignals))
	}
}

func (d *Detector) handleRemoteStart(signals []Signal) {
	// 简化：计算平均置信度（实际上都是1.0）
	var avgConf float64 = 0.0
	if len(signals) > 0 {
		total := 0.0
		for _, s := range signals {
			total += s.Confidence
		}
		avgConf = total / float64(len(signals))
	}

	session := &storage.RemoteSession{
		StartTime:  d.lastChange,
		Signals:    d.formatSignals(signals),
		Confidence: avgConf,
	}

	if err := d.storage.SaveSession(session); err != nil {
		log.Printf("保存会话失败: %v", err)
	}

	for _, s := range signals {
		rawSignal := &storage.RawSignal{
			Type:       s.Type,
			Name:       s.Name,
			Confidence: s.Confidence,
			RawData:    s.Source,
			DetectedAt: s.DetectedAt,
		}
		rawSignal.SetSessionID(session.ID)
		d.storage.SaveRawSignal(rawSignal)
	}

	log.Printf("远程会话开始: %s, 置信度: %.2f", session.ID, avgConf)

	// 发送通知
	if d.notifier != nil {
		// 将 detector.Signal 转换为 NotifierSignal
		notifierSignals := make([]NotifierSignal, len(signals))
		for i, s := range signals {
			notifierSignals[i] = notifierSignal{name: s.Name}
		}
		d.notifier.NotifyRemoteStart(notifierSignals)
	}
}

func (d *Detector) handleRemoteEnd() {
	openSession, _ := d.storage.GetOpenSession()
	var signals []Signal
	if openSession != nil {
		endTime := time.Now()
		duration := endTime.Sub(openSession.StartTime)
		d.storage.UpdateSessionEnd(openSession.ID, endTime, duration)
		log.Printf("远程会话结束: %s, 持续时间: %v", openSession.ID, duration)

		// 从会话记录中解析信号信息
		if openSession.Signals != "" {
			signalNames := d.parseSignals(openSession.Signals)
			for _, name := range signalNames {
				signals = append(signals, Signal{
					Name: name,
				})
			}
		}
	}

	// 发送通知
	if d.notifier != nil {
		// 将信号名称转换为 NotifierSignal
		notifierSignals := make([]NotifierSignal, len(signals))
		for i, s := range signals {
			notifierSignals[i] = notifierSignal{name: s.Name}
		}
		d.notifier.NotifyRemoteEnd(notifierSignals)
	}
}

// parseSignals 解析信号字符串为信号名称列表
func (d *Detector) parseSignals(signalsStr string) []string {
	if signalsStr == "" {
		return nil
	}
	// 信号以逗号分隔
	parts := strings.Split(signalsStr, ",")
	var names []string
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (d *Detector) formatSignals(signals []Signal) string {
	var names []string
	for _, s := range signals {
		names = append(names, s.Name)
	}
	if len(names) == 0 {
		return ""
	}
	return joinStrings(names, ", ")
}

func joinStrings(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for i := 1; i < len(strs); i++ {
		result += sep + strs[i]
	}
	return result
}

func (d *Detector) GetStatus() *DetectionResult {
	d.stateMutex.RLock()
	defer d.stateMutex.RUnlock()

	d.signalMutex.RLock()
	signals := d.signals
	d.signalMutex.RUnlock()

	openSession, _ := d.storage.GetOpenSession()

	// 简化：计算平均置信度
	var avgConf float64 = 0.0
	if len(signals) > 0 {
		total := 0.0
		for _, s := range signals {
			total += s.Confidence
		}
		avgConf = total / float64(len(signals))
	}

	result := &DetectionResult{
		RemoteActive: d.lastState,
		Signals:      signals,
		OverallConf:  avgConf,
	}

	if openSession != nil && d.lastState {
		result.StartTime = openSession.StartTime
		duration := time.Since(openSession.StartTime)
		result.Duration = formatDuration(duration)
	}

	return result
}

func (d *Detector) GetHistory(limit int) ([]storage.RemoteSession, error) {
	return d.storage.GetRecentSessions(limit)
}

// GetHistoryPaginated 分页获取历史记录
func (d *Detector) GetHistoryPaginated(page, pageSize int) ([]storage.RemoteSession, int64, error) {
	return d.storage.GetSessionsPaginated(page, pageSize)
}

func formatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}
