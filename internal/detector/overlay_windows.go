package detector

import (
	"encoding/json"
	"strings"
)

// 用户叠加层的 Config KV：与版本化规则集独立存储，避免被官方规则更新覆盖。
const (
	ConfigKeyCustomTools   = "custom_tools"   // []RemoteTool 的 JSON：用户录制的自定义工具
	ConfigKeyDisabledTools = "disabled_tools" // []string 的 JSON：用户取消关注的工具进程名（小写）
	ConfigKeyToolOverrides = "tool_overrides" // map[进程名(小写)]RemoteTool 的 JSON：用户对内置规则的修改（覆盖层）
)

// MonitoredTool 是合并后供前端展示的监控项：官方/自定义规则 + 来源 + 是否启用 + 图标。
type MonitoredTool struct {
	RemoteTool
	Source      string `json:"source"`                // builtin | custom
	Enabled     bool   `json:"enabled"`               // 是否在监控中（未被加入 disabled_tools）
	IconDataURI string `json:"iconDataURI,omitempty"` // 进程在运行时从 exe 提取的图标
}

// officialRules 读取当前生效规则集（内置/GitHub/手工导入）的规则数组。
func (d *Detector) officialRules() []RemoteTool {
	active, err := d.storage.GetActiveRuleSet()
	if err != nil || active == nil {
		return nil
	}
	rules, _ := ParseRules(active.Rules)
	return rules
}

// GetToolOverrides 读取用户对内置规则的修改（覆盖层），键为进程名小写。
func (d *Detector) GetToolOverrides() (map[string]RemoteTool, error) {
	raw, err := d.storage.GetConfig(ConfigKeyToolOverrides)
	if err != nil || raw == "" {
		return nil, err
	}
	var ov map[string]RemoteTool
	if err := json.Unmarshal([]byte(raw), &ov); err != nil {
		return nil, err
	}
	return ov, nil
}

func (d *Detector) setToolOverrides(ov map[string]RemoteTool) error {
	if ov == nil {
		ov = map[string]RemoteTool{}
	}
	b, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	return d.storage.SetConfig(ConfigKeyToolOverrides, string(b))
}

// applyOverrides 用覆盖层替换官方规则中同进程名的条目（不改变顺序）。
func applyOverrides(official []RemoteTool, ov map[string]RemoteTool) []RemoteTool {
	if len(ov) == 0 {
		return official
	}
	out := make([]RemoteTool, len(official))
	for i, t := range official {
		if o, ok := ov[strings.ToLower(t.ProcessName)]; ok {
			out[i] = o
		} else {
			out[i] = t
		}
	}
	return out
}

// rulesEqual 通过 JSON 序列化判断两条规则是否完全一致（字段顺序由结构体固定，序列化稳定）。
func rulesEqual(a, b RemoteTool) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// GetCustomTools 读取用户录制的自定义工具规则。
func (d *Detector) GetCustomTools() ([]RemoteTool, error) {
	raw, err := d.storage.GetConfig(ConfigKeyCustomTools)
	if err != nil || raw == "" {
		return nil, err
	}
	var tools []RemoteTool
	if err := json.Unmarshal([]byte(raw), &tools); err != nil {
		return nil, err
	}
	return tools, nil
}

func (d *Detector) setCustomTools(tools []RemoteTool) error {
	b, err := json.Marshal(tools)
	if err != nil {
		return err
	}
	return d.storage.SetConfig(ConfigKeyCustomTools, string(b))
}

// GetDisabledTools 读取用户取消关注的工具进程名（小写）。
func (d *Detector) GetDisabledTools() ([]string, error) {
	raw, err := d.storage.GetConfig(ConfigKeyDisabledTools)
	if err != nil || raw == "" {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, err
	}
	return names, nil
}

func (d *Detector) setDisabledTools(names []string) error {
	b, err := json.Marshal(names)
	if err != nil {
		return err
	}
	return d.storage.SetConfig(ConfigKeyDisabledTools, string(b))
}

// mergeUserOverlay 把官方规则与用户叠加层合并：
// 先用覆盖层替换内置规则的修改，再追加自定义工具（按进程名去重），最后过滤已禁用工具。
func (d *Detector) mergeUserOverlay(official []RemoteTool) []RemoteTool {
	overrides, _ := d.GetToolOverrides()
	official = applyOverrides(official, overrides)
	custom, _ := d.GetCustomTools()
	disabled, _ := d.GetDisabledTools()

	dset := make(map[string]bool, len(disabled))
	for _, n := range disabled {
		dset[strings.ToLower(n)] = true
	}

	merged := make([]RemoteTool, 0, len(official)+len(custom))
	seen := make(map[string]bool, len(official)+len(custom))
	appendTool := func(t RemoteTool) {
		key := strings.ToLower(t.ProcessName)
		if key != "" {
			if seen[key] {
				return
			}
			seen[key] = true
			if dset[key] {
				return // 用户取消关注
			}
		}
		merged = append(merged, t)
	}
	for _, t := range official {
		appendTool(t)
	}
	for _, t := range custom {
		appendTool(t)
	}
	return merged
}

// AddCustomTool 新增/覆盖一条自定义工具规则（按进程名去重），并热重载检测规则。
func (d *Detector) AddCustomTool(tool RemoteTool) error {
	tools, err := d.GetCustomTools()
	if err != nil {
		return err
	}
	key := strings.ToLower(tool.ProcessName)
	replaced := false
	for i := range tools {
		if strings.ToLower(tools[i].ProcessName) == key {
			tools[i] = tool
			replaced = true
			break
		}
	}
	if !replaced {
		tools = append(tools, tool)
	}
	if err := d.setCustomTools(tools); err != nil {
		return err
	}
	return d.ReloadRules()
}

// EffectiveRulesForEditor 返回供高级文本编辑器展示的"全部规则"：
// 内置规则（已应用用户覆盖）+ 自定义工具，按进程名去重；不应用"取消关注"过滤，使所有规则可见。
func (d *Detector) EffectiveRulesForEditor() []RemoteTool {
	overrides, _ := d.GetToolOverrides()
	official := applyOverrides(d.officialRules(), overrides)
	custom, _ := d.GetCustomTools()

	out := make([]RemoteTool, 0, len(official)+len(custom))
	seen := make(map[string]bool, len(official)+len(custom))
	add := func(t RemoteTool) {
		key := strings.ToLower(t.ProcessName)
		if key != "" {
			if seen[key] {
				return
			}
			seen[key] = true
		}
		out = append(out, t)
	}
	for _, t := range official {
		add(t)
	}
	for _, t := range custom {
		add(t)
	}
	return out
}

// SaveRulesFromEditor 保存高级文本编辑器提交的"全部规则"，并热重载：
//   - 进程名命中内置规则且内容有改动 → 记为覆盖层（可一键恢复默认、且不被官方更新冲掉）
//   - 进程名未命中内置规则 → 记为自定义工具
//
// 内置规则即便从编辑器删除也不会消失（下次仍按默认展示），如需停用请用"关注"开关；
// 自定义工具从编辑器删除即等于删除。
func (d *Detector) SaveRulesFromEditor(edited []RemoteTool) error {
	baseline := d.officialRules()
	bmap := make(map[string]RemoteTool, len(baseline))
	for _, t := range baseline {
		bmap[strings.ToLower(t.ProcessName)] = t
	}

	overrides := make(map[string]RemoteTool)
	custom := make([]RemoteTool, 0)
	seen := make(map[string]bool)
	for _, e := range edited {
		key := strings.ToLower(e.ProcessName)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if base, ok := bmap[key]; ok {
			if !rulesEqual(e, base) {
				overrides[key] = e
			}
		} else {
			custom = append(custom, e)
		}
	}

	if err := d.setToolOverrides(overrides); err != nil {
		return err
	}
	if err := d.setCustomTools(custom); err != nil {
		return err
	}
	return d.ReloadRules()
}

// ResetBuiltinOverrides 清空对内置规则的全部修改，恢复官方默认（不影响自定义工具）。
func (d *Detector) ResetBuiltinOverrides() error {
	if err := d.setToolOverrides(nil); err != nil {
		return err
	}
	return d.ReloadRules()
}

// RemoveCustomTool 按进程名删除一条自定义工具规则，并热重载检测规则。
func (d *Detector) RemoveCustomTool(processName string) error {
	tools, err := d.GetCustomTools()
	if err != nil {
		return err
	}
	key := strings.ToLower(processName)
	kept := tools[:0]
	for _, t := range tools {
		if strings.ToLower(t.ProcessName) != key {
			kept = append(kept, t)
		}
	}
	if err := d.setCustomTools(kept); err != nil {
		return err
	}
	return d.ReloadRules()
}

// SetToolEnabled 设置某工具是否被监控（写入/移出 disabled_tools），并热重载检测规则。
func (d *Detector) SetToolEnabled(processName string, enabled bool) error {
	disabled, err := d.GetDisabledTools()
	if err != nil {
		return err
	}
	key := strings.ToLower(processName)
	out := disabled[:0]
	present := false
	for _, n := range disabled {
		if strings.ToLower(n) == key {
			present = true
			continue // 先全部移除，下面按需重新加入
		}
		out = append(out, n)
	}
	if !enabled {
		out = append(out, key)
	}
	if enabled && !present {
		return nil // 本就启用，无需改动
	}
	if err := d.setDisabledTools(out); err != nil {
		return err
	}
	return d.ReloadRules()
}

// ListMonitoredTools 返回"官方规则 + 自定义工具"的合并清单（去重），每条带来源、是否启用与图标。
func (d *Detector) ListMonitoredTools() ([]MonitoredTool, error) {
	overrides, _ := d.GetToolOverrides()
	official := applyOverrides(d.officialRules(), overrides)
	custom, _ := d.GetCustomTools()
	disabled, _ := d.GetDisabledTools()
	dset := make(map[string]bool, len(disabled))
	for _, n := range disabled {
		dset[strings.ToLower(n)] = true
	}

	seen := make(map[string]bool)
	out := make([]MonitoredTool, 0, len(official)+len(custom))
	add := func(t RemoteTool, source string) {
		key := strings.ToLower(t.ProcessName)
		if key != "" && seen[key] {
			return
		}
		seen[key] = true
		out = append(out, MonitoredTool{
			RemoteTool:  t,
			Source:      source,
			Enabled:     !dset[key],
			IconDataURI: d.toolIcon(t.ProcessName),
		})
	}
	for _, t := range official {
		add(t, "builtin")
	}
	for _, t := range custom {
		add(t, "custom")
	}
	return out, nil
}

// toolIcon 若该工具进程正在运行，则从其 exe 提取图标；否则返回空串。
func (d *Detector) toolIcon(processName string) string {
	if processName == "" {
		return ""
	}
	d.windowsMu.Lock()
	procs := d.windows.findProcessesByName(processName)
	d.windowsMu.Unlock()
	for _, p := range procs {
		if exe, err := p.Exe(); err == nil && exe != "" {
			if uri := ExtractIconDataURI(exe); uri != "" {
				return uri
			}
		}
	}
	return ""
}

// SnapshotProcesses 拍一张进程快照（供录制基线/对比使用）。
func (d *Detector) SnapshotProcesses() map[int32]ProcSnap {
	d.windowsMu.Lock()
	defer d.windowsMu.Unlock()
	return d.windows.SnapshotProcesses()
}

// DiffSnapshots 以传入的基线快照与当前实时快照做差集，返回疑似远程工具候选。
func (d *Detector) DiffSnapshots(baseline map[int32]ProcSnap) []Candidate {
	d.windowsMu.Lock()
	defer d.windowsMu.Unlock()
	after := d.windows.SnapshotProcesses()
	return d.windows.DiffSnapshots(baseline, after)
}
