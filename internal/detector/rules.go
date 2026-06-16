package detector

import "encoding/json"

// RemoteTool 定义远程工具配置（检测规则）。
//
// 该结构体同时用于三处，必须保持 JSON 标签一致：
//   - 守护进程内置默认规则 defaultRules（首次运行写入 SQLite 的种子）
//   - SQLite detection_rule_sets 表中存储的规则 JSON
//   - GitHub 发布的 data/rules.json
//
// 向前兼容：反序列化时未知字段会被忽略，因此 GitHub 上的新版本规则即便包含
// 旧 exe 不认识的新指标字段，旧 exe 也不会反序列化失败——但旧 exe 会忽略这些新
// 指标导致漏检，所以发布带新指标的规则时必须同步抬高 version.json 的 minAppVersion。
type RemoteTool struct {
	ProcessName             string   `json:"processName"`                       // 进程名（可为空，表示不检查进程名）
	WindowClass             string   `json:"windowClass,omitempty"`             // 窗口类名（用于检测远程状态）
	WindowTitle             string   `json:"windowTitle,omitempty"`             // 窗口标题（用于检测远程状态，支持部分匹配）
	CommandLineArgs         []string `json:"commandLineArgs,omitempty"`         // 命令行参数特征（用于检测远程状态）
	DetectChildProcess      bool     `json:"detectChildProcess,omitempty"`      // 是否检测"会话子进程"：父进程也是同名进程的派生进程（新版 ToDesk 会话激活时派生）
	ChildProcessExcludeArgs []string `json:"childProcessExcludeArgs,omitempty"` // 子进程检测时需排除的命令行特征（用于排除常驻的服务/主客户端进程）
	ToolName                string   `json:"toolName"`                          // 工具显示名称
	TCPConnThreshold        int      `json:"tcpConnThreshold,omitempty"`        // TCP连接数阈值（大于等于此值认为被远程，0表示不检测）
	UDPConnThreshold        int      `json:"udpConnThreshold,omitempty"`        // UDP连接数阈值（大于此值认为被远程，0表示不检测）
	UseEstablishedOnly      bool     `json:"useEstablishedOnly,omitempty"`      // 是否只统计ESTABLISHED状态的连接（仅对TCP有效）
}

// DefaultRulesVersion 是内置默认规则的版本号，首次运行时作为种子写入 SQLite。
const DefaultRulesVersion = "1.0.0"

// defaultRules 内置默认检测规则（即"现在的规则"），首次运行时写入 SQLite 作为种子基线。
// 该列表必须与仓库根 data/rules.json（v1.0.0）的内容保持一致。
var defaultRules = []RemoteTool{
	// ToDesk 兼容两种检测方式：
	//   旧版：主客户端命令行同时带 --localPort= 和 --isVideoSession=true
	//   新版：远程会话激活时，在主客户端下派生一个无参数的 ToDesk.exe 子进程（排除带 --runservice/--localPort 的常驻进程）
	{ProcessName: "todesk.exe", CommandLineArgs: []string{"--localPort=", "--isVideoSession=true"}, DetectChildProcess: true, ChildProcessExcludeArgs: []string{"--runservice", "--localPort", "--isVideoSession", "--hide"}, ToolName: "ToDesk"},
	{ProcessName: "AweSun.exe", CommandLineArgs: []string{"--mod=desktopagent", "--port=", "--agentid=", "--lockscreen="}, ToolName: "向日葵"}, // 向日葵使用命令行参数检测
	{ProcessName: "sunloginclient.exe", ToolName: "向日葵客户端"},                                                  // 占位符，待补充检测方法
	{ProcessName: "GameViewerServer.exe", ToolName: "网易UU远程", TCPConnThreshold: 5, UseEstablishedOnly: true}, // 网易UU远程，基于TCP连接数检测
	{ProcessName: "AskLink.exe", ToolName: "AskLink远程", UDPConnThreshold: 1},                                 // AskLink远程，基于UDP连接数检测
	{ProcessName: "RCClient.exe", ToolName: "远程看看", WindowTitle: "聊天"},                                       // 远程看看，基于窗口标题检测
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
