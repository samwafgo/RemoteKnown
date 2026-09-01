package detector

import (
	"encoding/json"
	"strings"
)

// RemoteTool 定义远程工具配置（检测规则）。
//
// 该结构体同时用于三处，必须保持 JSON 标签一致：
//   - 守护进程内置默认规则 defaultRules（首次运行写入 SQLite 的种子）
//   - SQLite detection_rule_sets 表中存储的规则 JSON
//   - GitHub 发布的 data/rules.json
//
// 规则表是"扁平"的：同一个进程允许出现多条规则，用来覆盖同一款远程软件不同版本
// 的不同检测特征（例如向日葵旧版是 --mod=desktopagent、新版多了 --mod=dragserver）。
// 这些规则共用同一个 ToolName（对外仍是同一个工具，通知与历史记录不会分裂），
// 各自用 Remark 说明自己针对哪种情况。
//
// 向前兼容：反序列化时未知字段会被忽略，因此 GitHub 上的新版本规则即便包含
// 旧 exe 不认识的新指标字段，旧 exe 也不会反序列化失败——但旧 exe 会忽略这些新
// 指标导致漏检，所以发布带新指标的规则时必须同步抬高 version.json 的 minAppVersion。
// 同理，v1.0.8 之前的 exe 会把同进程的多条规则去重成一条（漏检，不会误报），
// 所以含同进程多规则的 rules.json 其 minAppVersion 不得低于 1.0.8。
type RemoteTool struct {
	ID                      string   `json:"id,omitempty"`                      // 规则稳定标识；用户覆盖层/编辑器 diff 的主键，留空时回落为 "进程名|工具名"
	ProcessName             string   `json:"processName"`                       // 进程名（可为空，表示不检查进程名）
	ToolName                string   `json:"toolName"`                          // 工具显示名称；同一工具的多条规则应保持一致
	Remark                  string   `json:"remark,omitempty"`                  // 备注：说明本条规则针对哪个版本/哪种场景，不参与匹配
	WindowClass             string   `json:"windowClass,omitempty"`             // 窗口类名（用于检测远程状态）
	WindowTitle             string   `json:"windowTitle,omitempty"`             // 窗口标题（用于检测远程状态，支持部分匹配）
	CommandLineArgs         []string `json:"commandLineArgs,omitempty"`         // 命令行参数特征（用于检测远程状态）
	DetectChildProcess      bool     `json:"detectChildProcess,omitempty"`      // 是否检测"会话子进程"：父进程也是同名进程的派生进程（新版 ToDesk 会话激活时派生）
	ChildProcessExcludeArgs []string `json:"childProcessExcludeArgs,omitempty"` // 子进程检测时需排除的命令行特征（用于排除常驻的服务/主客户端进程）
	TCPConnThreshold        int      `json:"tcpConnThreshold,omitempty"`        // TCP连接数阈值（大于等于此值认为被远程，0表示不检测）
	UDPConnThreshold        int      `json:"udpConnThreshold,omitempty"`        // UDP连接数阈值（大于此值认为被远程，0表示不检测）
	UseEstablishedOnly      bool     `json:"useEstablishedOnly,omitempty"`      // 是否只统计ESTABLISHED状态的连接（仅对TCP有效）
}

// ToolKey 是"工具身份"：进程名 + 工具名，同一工具的多条规则得到同一个 key。
// 用于监控对象列表的分组与「关注」开关（disabled_tools）。
func ToolKey(t RemoteTool) string {
	return strings.ToLower(t.ProcessName) + "|" + strings.ToLower(t.ToolName)
}

// RuleKey 是"规则身份"：优先用 ID，缺省回落到工具身份。
// 用于规则去重、用户覆盖层（tool_overrides）与编辑器 diff。
func RuleKey(t RemoteTool) string {
	if t.ID != "" {
		return strings.ToLower(t.ID)
	}
	return ToolKey(t)
}

// legacyKey 是 v1.0.7 及更早版本使用的主键（仅进程名）。
// 读取用户存量的 tool_overrides / disabled_tools 时用它做兼容回落。
func legacyKey(t RemoteTool) string {
	return strings.ToLower(t.ProcessName)
}

// DefaultRulesVersion 是内置默认规则的版本号，首次运行时作为种子写入 SQLite。
const DefaultRulesVersion = "1.0.2"

// defaultRules 内置默认检测规则（即"现在的规则"），首次运行时写入 SQLite 作为种子基线。
// 该列表必须与仓库根 data/rules.json（v1.0.2）的内容保持一致。
var defaultRules = []RemoteTool{
	// ToDesk 兼容两种检测方式：
	//   旧版：主客户端命令行同时带 --localPort= 和 --isVideoSession=true
	//   新版：远程会话激活时，在主客户端下派生一个无参数的 ToDesk.exe 子进程（排除带 --runservice/--localPort 的常驻进程）
	{ID: "todesk-session", ProcessName: "todesk.exe", ToolName: "ToDesk", Remark: "会话进程（命令行参数 / 会话子进程）", CommandLineArgs: []string{"--localPort=", "--isVideoSession=true"}, DetectChildProcess: true, ChildProcessExcludeArgs: []string{"--runservice", "--localPort", "--isVideoSession", "--hide"}},
	// 向日葵同进程两条规则：远程桌面会话与文件拖拽会话的命令行特征不同，任一命中即算被远程
	{ID: "awesun-desktopagent", ProcessName: "AweSun.exe", ToolName: "向日葵", Remark: "远程桌面会话（desktopagent）", CommandLineArgs: []string{"--mod=desktopagent", "--port=", "--agentid=", "--lockscreen="}},
	{ID: "awesun-dragserver", ProcessName: "AweSun.exe", ToolName: "向日葵", Remark: "文件拖拽会话（dragserver，新版）", CommandLineArgs: []string{"--mod=dragserver", "--port=", "--dragid="}},
	{ID: "sunlogin-client", ProcessName: "sunloginclient.exe", ToolName: "向日葵客户端", Remark: "进程存在即判定（待补充更精确特征）"},
	{ID: "uu-gameviewer", ProcessName: "GameViewerServer.exe", ToolName: "网易UU远程", Remark: "TCP 连接数达到阈值", TCPConnThreshold: 5, UseEstablishedOnly: true},
	{ID: "asklink", ProcessName: "AskLink.exe", ToolName: "AskLink远程", Remark: "UDP 连接数超过阈值", UDPConnThreshold: 1},
	{ID: "rcclient", ProcessName: "RCClient.exe", ToolName: "远程看看", Remark: "会话中出现「聊天」窗口", WindowTitle: "聊天"},
}

// DefaultRulesJSON 将内置默认规则序列化为 JSON 数组文本，供首次种子写入 SQLite。
func DefaultRulesJSON() (string, error) {
	b, err := json.Marshal(defaultRules)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseRules 将存储/下载的规则 JSON 数组文本反序列化为 []RemoteTool。
func ParseRules(rulesJSON string) ([]RemoteTool, error) {
	var rules []RemoteTool
	if err := json.Unmarshal([]byte(rulesJSON), &rules); err != nil {
		return nil, err
	}
	return rules, nil
}
