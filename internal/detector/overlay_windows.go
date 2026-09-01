package detector

import (
	"encoding/json"
	"strings"
)

// 用户叠加层的 Config KV：与版本化规则集独立存储，避免被官方规则更新覆盖。
const (
	ConfigKeyCustomTools   = "custom_tools"   // []RemoteTool 的 JSON：用户录制的自定义工具
	ConfigKeyDisabledTools = "disabled_tools" // []string 的 JSON：用户取消关注的工具键（ToolKey，兼容旧的纯进程名）
	ConfigKeyToolOverrides = "tool_overrides" // map[RuleKey]RemoteTool 的 JSON：用户对内置规则的修改（覆盖层，兼容旧的纯进程名键）
)

// MonitoredRule 是监控对象卡片下的一条具体检测规则。
type MonitoredRule struct {
	RemoteTool
	Source string `json:"source"` // builtin | custom
}

// MonitoredTool 是合并后供前端展示的监控项：按「工具身份」（进程名 + 工具名）聚合，
// 同一工具下针对不同版本/场景的多条规则收在 Rules 里，各自带 remark 备注。
type MonitoredTool struct {
	ProcessName string          `json:"processName"`
	ToolName    string          `json:"toolName"`
	Source      string          `json:"source"`                // builtin（含任一内置规则）| custom（全部由用户录制）
	Enabled     bool            `json:"enabled"`               // 是否在监控中（未被加入 disabled_tools）
	IconDataURI string          `json:"iconDataURI,omitempty"` // 进程在运行时从 exe 提取的图标
	Rules       []MonitoredRule `json:"rules"`                 // 该工具下的全部检测规则
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

// GetToolOverrides 读取用户对内置规则的修改（覆盖层），键为 RuleKey（存量数据可能是纯进程名）。
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

// applyOverrides 用覆盖层替换官方规则中同 RuleKey 的条目（不改变顺序）。
//
// 兼容 v1.0.7 及更早版本按纯进程名存的覆盖：这类键只作用于该进程名的第一条规则
// （即当年存覆盖时唯一存在的那条），避免把后来新增的同进程规则一起冲掉。
func applyOverrides(official []RemoteTool, ov map[string]RemoteTool) []RemoteTool {
	if len(ov) == 0 {
		return official
	}
	legacyUsed := make(map[string]bool)
	out := make([]RemoteTool, len(official))
	for i, t := range official {
		if o, ok := ov[RuleKey(t)]; ok {
			out[i] = o
			continue
		}
		lk := legacyKey(t)
		if o, ok := ov[lk]; ok && !legacyUsed[lk] {
			legacyUsed[lk] = true
			out[i] = o
			continue
		}
		out[i] = t
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

// GetDisabledTools 读取用户取消关注的工具键（ToolKey，存量数据可能是纯进程名）。
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

// disabledSet 把存储的「取消关注」列表读成集合，元素既可能是 ToolKey 也可能是旧的纯进程名。
func (d *Detector) disabledSet() map[string]bool {
	disabled, _ := d.GetDisabledTools()
	set := make(map[string]bool, len(disabled))
	for _, n := range disabled {
		set[strings.ToLower(n)] = true
	}
	return set
}

// isDisabled 判断一条规则所属的工具是否被用户取消关注（兼容旧的纯进程名键）。
func isDisabled(set map[string]bool, t RemoteTool) bool {
	return set[ToolKey(t)] || set[legacyKey(t)]
}

// mergeUserOverlay 把官方规则与用户叠加层合并：
// 先用覆盖层替换内置规则的修改，再追加自定义工具（按 RuleKey 去重），最后过滤已取消关注的工具。
//
// 注意去重用的是「规则身份」而不是进程名：同一进程可以保留多条规则（同一工具的不同版本特征），
// 检测时任一命中即判定为被远程。
func (d *Detector) mergeUserOverlay(official []RemoteTool) []RemoteTool {
	overrides, _ := d.GetToolOverrides()
	official = applyOverrides(official, overrides)
	custom, _ := d.GetCustomTools()
	dset := d.disabledSet()

	merged := make([]RemoteTool, 0, len(official)+len(custom))
	seen := make(map[string]bool, len(official)+len(custom))
	appendTool := func(t RemoteTool) {
		key := RuleKey(t)
		if seen[key] {
			return
		}
		seen[key] = true
		if isDisabled(dset, t) {
			return // 用户取消关注
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

// AddCustomTool 新增/覆盖一条自定义工具规则（按 RuleKey 去重），并热重载检测规则。
func (d *Detector) AddCustomTool(tool RemoteTool) error {
	tools, err := d.GetCustomTools()
	if err != nil {
		return err
	}
	key := RuleKey(tool)
	replaced := false
	for i := range tools {
		if RuleKey(tools[i]) == key {
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

// EffectiveRulesForEditor 返回供高级文本编辑器展示的「全部规则」：
// 内置规则（已应用用户覆盖）+ 自定义工具，按 RuleKey 去重；不应用「取消关注」过滤，使所有规则可见。
func (d *Detector) EffectiveRulesForEditor() []RemoteTool {
	overrides, _ := d.GetToolOverrides()
	official := applyOverrides(d.officialRules(), overrides)
	custom, _ := d.GetCustomTools()

	out := make([]RemoteTool, 0, len(official)+len(custom))
	seen := make(map[string]bool, len(official)+len(custom))
	add := func(t RemoteTool) {
		key := RuleKey(t)
		if seen[key] {
			return
		}
		seen[key] = true
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

// SaveRulesFromEditor 保存高级文本编辑器提交的「全部规则」，并热重载：
//   - RuleKey 命中内置规则且内容有改动 → 记为覆盖层（可一键恢复默认、且不被官方更新冲掉）
//   - RuleKey 未命中内置规则 → 记为自定义工具
//
// 内置规则即便从编辑器删除也不会消失（下次仍按默认展示），如需停用请用「关注」开关；
// 自定义工具从编辑器删除即等于删除。
//
// 内置规则带 id，因此在编辑器里改工具名/进程名也仍能对上基线记为覆盖；
// 没有 id 的规则以「进程名|工具名」为身份，改这两项等于换成了一条新规则。
func (d *Detector) SaveRulesFromEditor(edited []RemoteTool) error {
	baseline := d.officialRules()
	bmap := make(map[string]RemoteTool, len(baseline))
	for _, t := range baseline {
		bmap[RuleKey(t)] = t
	}

	overrides := make(map[string]RemoteTool)
	custom := make([]RemoteTool, 0)
	seen := make(map[string]bool)
	for _, e := range edited {
		key := RuleKey(e)
		if e.ProcessName == "" || seen[key] {
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

// RemoveCustomTool 删除某个自定义工具的全部规则（按工具身份匹配），并热重载检测规则。
// toolName 留空时退化为按进程名删除（兼容旧调用）。
func (d *Detector) RemoveCustomTool(processName, toolName string) error {
	tools, err := d.GetCustomTools()
	if err != nil {
		return err
	}
	pn := strings.ToLower(processName)
	tn := strings.ToLower(toolName)
	kept := tools[:0]
	for _, t := range tools {
		match := strings.ToLower(t.ProcessName) == pn &&
			(tn == "" || strings.ToLower(t.ToolName) == tn)
		if !match {
			kept = append(kept, t)
		}
	}
	if err := d.setCustomTools(kept); err != nil {
		return err
	}
	return d.ReloadRules()
}

// SetToolEnabled 设置某工具是否被监控（写入/移出 disabled_tools），并热重载检测规则。
// 作用于整个工具（该工具下的全部规则一起启停）。toolName 留空时退化为按进程名操作。
func (d *Detector) SetToolEnabled(processName, toolName string, enabled bool) error {
	disabled, err := d.GetDisabledTools()
	if err != nil {
		return err
	}
	key := ToolKey(RemoteTool{ProcessName: processName, ToolName: toolName})
	legacy := strings.ToLower(processName)

	out := disabled[:0]
	present := false
	for _, n := range disabled {
		lower := strings.ToLower(n)
		// 同时清掉新键与旧的纯进程名键，避免残留导致关不掉/开不了
		if lower == key || lower == legacy {
			present = true
			continue
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

// ListMonitoredTools 返回「官方规则 + 自定义工具」的监控清单，按工具身份（进程名 + 工具名）聚合：
// 同一工具下的多条规则收在 Rules 里（各带 remark 备注），关注开关与图标作用于整个工具。
func (d *Detector) ListMonitoredTools() ([]MonitoredTool, error) {
	overrides, _ := d.GetToolOverrides()
	official := applyOverrides(d.officialRules(), overrides)
	custom, _ := d.GetCustomTools()
	dset := d.disabledSet()

	out := make([]MonitoredTool, 0, len(official)+len(custom))
	idx := make(map[string]int)       // ToolKey → out 下标
	seenRule := make(map[string]bool) // RuleKey 去重
	icons := make(map[string]string)  // 进程名 → 图标，避免同工具多条规则重复提取

	add := func(t RemoteTool, source string) {
		rk := RuleKey(t)
		if seenRule[rk] {
			return
		}
		seenRule[rk] = true

		tk := ToolKey(t)
		i, ok := idx[tk]
		if !ok {
			pn := strings.ToLower(t.ProcessName)
			icon, cached := icons[pn]
			if !cached {
				icon = d.toolIcon(t.ProcessName)
				icons[pn] = icon
			}
			out = append(out, MonitoredTool{
				ProcessName: t.ProcessName,
				ToolName:    t.ToolName,
				Source:      source,
				Enabled:     !isDisabled(dset, t),
				IconDataURI: icon,
			})
			i = len(out) - 1
			idx[tk] = i
		} else if source == "builtin" {
			out[i].Source = "builtin" // 工具下只要有内置规则，就不允许整组删除
		}
		out[i].Rules = append(out[i].Rules, MonitoredRule{RemoteTool: t, Source: source})
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
