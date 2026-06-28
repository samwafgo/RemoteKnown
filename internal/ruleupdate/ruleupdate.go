// Package ruleupdate 负责从 GitHub raw 拉取检测规则的版本信息与规则内容，并提供版本比较。
// 仅使用标准库 net/http，无新第三方依赖。
package ruleupdate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// httpClient 带超时，避免 GitHub 不可达时长时间阻塞。
var httpClient = &http.Client{Timeout: 15 * time.Second}

// VersionInfo 对应 data/version.json。
type VersionInfo struct {
	Version       string `json:"version"`
	MinAppVersion string `json:"minAppVersion"`
	Description   string `json:"description"`
	UpdatedAt     string `json:"updatedAt"`
}

// rulesFile 对应 data/rules.json 的结构。
type rulesFile struct {
	Version       string            `json:"version"`
	MinAppVersion string            `json:"minAppVersion"`
	Tools         []json.RawMessage `json:"tools"`
}

// FetchVersion 拉取 baseURL/version.json 并解析。
func FetchVersion(baseURL string) (*VersionInfo, error) {
	body, err := httpGet(strings.TrimRight(baseURL, "/") + "/version.json")
	if err != nil {
		return nil, err
	}
	var info VersionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("解析 version.json 失败: %w", err)
	}
	if info.Version == "" {
		return nil, fmt.Errorf("version.json 缺少 version 字段")
	}
	return &info, nil
}

// FetchRules 拉取 baseURL/rules.json 并解析（自动更新路径）。
// 返回的 rulesJSON 与 SQLite 中存储、detector.ParseRules 解析的格式一致（[]RemoteTool）。
func FetchRules(baseURL string) (version, minAppVersion, rulesJSON string, err error) {
	body, err := httpGet(strings.TrimRight(baseURL, "/") + "/rules.json")
	if err != nil {
		return "", "", "", err
	}
	return ParseRulesContent(body)
}

// ParseRulesContent 解析一份 rules.json 内容（{version, minAppVersion, tools}）。
// 自动更新（FetchRules）与手工导入（人工上传 rules.json）共用此函数，保证格式一致。
func ParseRulesContent(data []byte) (version, minAppVersion, rulesJSON string, err error) {
	var rf rulesFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return "", "", "", fmt.Errorf("解析 rules.json 失败: %w", err)
	}
	if rf.Version == "" {
		return "", "", "", fmt.Errorf("rules.json 缺少 version 字段")
	}
	if len(rf.Tools) == 0 {
		return "", "", "", fmt.Errorf("rules.json 的 tools 为空")
	}
	toolsJSON, err := json.Marshal(rf.Tools)
	if err != nil {
		return "", "", "", err
	}
	return rf.Version, rf.MinAppVersion, string(toolsJSON), nil
}

func httpGet(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("请求 %s 返回状态码 %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// CompareVersions 比较两个点分版本号（如 "1.2.0" 与 "1.10"）。
// 返回 -1 (a<b)、0 (a==b)、1 (a>b)。缺位的段按 0 处理，非数字段按 0 处理。
func CompareVersions(a, b string) int {
	as := strings.Split(strings.TrimSpace(a), ".")
	bs := strings.Split(strings.TrimSpace(b), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av = atoiSafe(as[i])
		}
		if i < len(bs) {
			bv = atoiSafe(bs[i])
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

// IsNewer 报告 remote 版本是否比 current 更新。
func IsNewer(remote, current string) bool {
	return CompareVersions(remote, current) > 0
}

// atoiSafe 取字符串的前导数字部分，忽略预发布后缀（如 "7-beta" → 7、"beta" → 0）。
// 这样 beta 版守护进程（如 1.0.7-beta.1）在 minAppVersion 门槛比较时按其基础版本 1.0.7 计，
// 不会被要求 1.0.7 的规则误判为版本过低。
func atoiSafe(s string) int {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	v, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return v
}
