package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"RemoteKnown/internal/detector"
	"RemoteKnown/internal/notifier"
	"RemoteKnown/internal/ruleupdate"
	"RemoteKnown/internal/storage"
	"RemoteKnown/internal/version"
)

const (
	DefaultPort = 18080

	// defaultRulesUpdateURL 是检测规则的 GitHub raw 发布源（可通过配置项 rules_update_url 覆盖）。
	defaultRulesUpdateURL = "https://raw.githubusercontent.com/samwafgo/RemoteKnown/main/data"
)

type Server struct {
	detector *detector.Detector
	storage  *storage.Storage
	notifier *notifier.Notifier
	port     int
	running  bool
	clients  map[string]chan []byte
	clientMu sync.RWMutex
}

type StatusResponse struct {
	RemoteActive bool              `json:"remote_active"`
	StartTime    string            `json:"start_time,omitempty"`
	Duration     string            `json:"duration,omitempty"`
	Signals      []detector.Signal `json:"signals"`
	OverallConf  float64           `json:"overall_confidence"`
}

func NewServer(detector *detector.Detector, storage *storage.Storage, notifier *notifier.Notifier) *Server {
	return &Server{
		detector: detector,
		storage:  storage,
		notifier: notifier,
		port:     DefaultPort,
		clients:  make(map[string]chan []byte),
	}
}

func (s *Server) Start() error {
	http.HandleFunc("/api/status", s.handleStatus)
	http.HandleFunc("/api/history", s.handleHistory)
	http.HandleFunc("/api/config", s.handleConfig)
	http.HandleFunc("/api/notification", s.handleNotification)
	http.HandleFunc("/api/notification/test", s.handleTestNotification)
	http.HandleFunc("/api/notify", s.handleNotify)
	http.HandleFunc("/api/device-name", s.handleDeviceName)
	http.HandleFunc("/api/rules/version", s.handleRulesVersion)
	http.HandleFunc("/api/rules/check", s.handleRulesCheck)
	http.HandleFunc("/api/rules/apply", s.handleRulesApply)
	http.HandleFunc("/api/rules/upload", s.handleRulesUpload)
	http.HandleFunc("/api/rules/rollback", s.handleRulesRollback)
	http.HandleFunc("/health", s.handleHealth)

	s.running = true

	addr := s.getListenAddr()
	log.Printf("启动 HTTP API 服务器: %s", addr)

	return http.ListenAndServe(addr, nil)
}

func (s *Server) getListenAddr() string {
	return ":" + strconv.Itoa(s.port)
}

func (s *Server) Stop() {
	s.running = false
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	result := s.detector.GetStatus()
	response := StatusResponse{
		RemoteActive: result.RemoteActive,
		Signals:      result.Signals,
		OverallConf:  result.OverallConf,
	}

	if !result.StartTime.IsZero() {
		response.StartTime = result.StartTime.Format(time.RFC3339)
	}
	response.Duration = result.Duration

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 解析分页参数
	page := 1
	pageSize := 5 // 默认每页5条

	if pageStr := r.URL.Query().Get("page"); pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}

	if pageSizeStr := r.URL.Query().Get("pageSize"); pageSizeStr != "" {
		if ps, err := strconv.Atoi(pageSizeStr); err == nil && ps > 0 {
			pageSize = ps
		}
	}

	sessions, total, err := s.detector.GetHistoryPaginated(page, pageSize)
	if err != nil {
		http.Error(w, "获取历史记录失败", http.StatusInternalServerError)
		return
	}

	// 返回分页结果
	response := map[string]interface{}{
		"sessions":   sessions,
		"total":      total,
		"page":       page,
		"pageSize":   pageSize,
		"totalPages": (int(total) + pageSize - 1) / pageSize, // 向上取整
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		key := r.URL.Query().Get("key")
		if key == "" {
			http.Error(w, "key is required", http.StatusBadRequest)
			return
		}
		value, err := s.storage.GetConfig(key)
		if err != nil {
			http.Error(w, "获取配置失败", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"value": value})

	case http.MethodPost:
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if key, ok := req["key"]; ok {
			if value, ok := req["value"]; ok {
				s.storage.SetConfig(key, value)
			}
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleNotification(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 尝试获取新格式的通知配置
		allConfigJSON, err := s.storage.GetConfig("notification_configs")
		if err != nil {
			log.Printf("获取通知配置失败: %v", err)
			http.Error(w, "获取通知配置失败", http.StatusInternalServerError)
			return
		}

		var allConfigs map[string]interface{}

		// 如果新格式配置不存在，尝试从旧格式迁移
		if allConfigJSON == "" {
			oldConfigJSON, _ := s.storage.GetConfig("notification_config")
			if oldConfigJSON != "" {
				log.Printf("检测到旧格式配置，开始迁移...")
				var oldConfig map[string]interface{}
				if err := json.Unmarshal([]byte(oldConfigJSON), &oldConfig); err == nil {
					// 从旧格式迁移到新格式
					allConfigs = make(map[string]interface{})
					allConfigs["enabled"] = oldConfig["enabled"]
					allConfigs["type"] = oldConfig["type"]

					// 将旧配置保存到对应类型的子项中
					configType := "feishu"
					if t, ok := oldConfig["type"].(string); ok {
						configType = t
					}

					allConfigs[configType] = map[string]interface{}{
						"webhook_url": oldConfig["webhook_url"],
						"secret":      oldConfig["secret"],
					}

					// 初始化其他类型的默认配置
					if configType != "feishu" {
						allConfigs["feishu"] = map[string]interface{}{
							"webhook_url": "",
							"secret":      "",
						}
					}
					if configType != "dingtalk" {
						allConfigs["dingtalk"] = map[string]interface{}{
							"webhook_url": "",
							"secret":      "",
						}
					}

					// 保存迁移后的新格式配置
					if newJSON, err := json.Marshal(allConfigs); err == nil {
						s.storage.SetConfig("notification_configs", string(newJSON))
						log.Printf("配置迁移完成")
					}
				}
			}
		} else {
			// 解析现有配置
			if err := json.Unmarshal([]byte(allConfigJSON), &allConfigs); err != nil {
				log.Printf("解析配置失败: %v", err)
				http.Error(w, "解析配置失败", http.StatusInternalServerError)
				return
			}
		}

		// 如果还是没有配置，返回默认配置
		if allConfigs == nil {
			allConfigs = map[string]interface{}{
				"enabled": false,
				"type":    "feishu",
				"feishu": map[string]interface{}{
					"webhook_url": "",
					"secret":      "",
				},
				"dingtalk": map[string]interface{}{
					"webhook_url": "",
					"secret":      "",
				},
			}
		} else {
			// 确保包含所有必需的字段
			if allConfigs["feishu"] == nil {
				allConfigs["feishu"] = map[string]interface{}{
					"webhook_url": "",
					"secret":      "",
				}
			}
			if allConfigs["dingtalk"] == nil {
				allConfigs["dingtalk"] = map[string]interface{}{
					"webhook_url": "",
					"secret":      "",
				}
			}
		}

		// 返回完整配置结构
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(allConfigs)

	case http.MethodPost:
		// 保存通知配置
		var newConfig struct {
			Enabled    bool                   `json:"enabled"`
			Type       string                 `json:"type"`
			WebhookURL string                 `json:"webhook_url"`
			Secret     string                 `json:"secret"`
			Email      map[string]interface{} `json:"email"`
		}

		if err := json.NewDecoder(r.Body).Decode(&newConfig); err != nil {
			log.Printf("解析通知配置请求失败: %v", err)
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// 读取现有的完整配置
		allConfigJSON, _ := s.storage.GetConfig("notification_configs")
		var allConfigs map[string]interface{}

		if allConfigJSON != "" {
			json.Unmarshal([]byte(allConfigJSON), &allConfigs)
		}

		if allConfigs == nil {
			allConfigs = make(map[string]interface{})
		}

		// 更新基本配置
		allConfigs["enabled"] = newConfig.Enabled
		allConfigs["type"] = newConfig.Type

		// 更新对应类型的具体配置：邮件存到 email 子项，飞书/钉钉存 webhook_url+secret
		if newConfig.Type == "email" {
			if newConfig.Email != nil {
				allConfigs["email"] = newConfig.Email
			}
		} else {
			allConfigs[newConfig.Type] = map[string]interface{}{
				"webhook_url": newConfig.WebhookURL,
				"secret":      newConfig.Secret,
			}
		}

		// 保存完整配置
		configJSON, err := json.Marshal(allConfigs)
		if err != nil {
			log.Printf("通知配置序列化失败: %v", err)
			http.Error(w, "配置序列化失败", http.StatusInternalServerError)
			return
		}

		if err := s.storage.SetConfig("notification_configs", string(configJSON)); err != nil {
			log.Printf("保存通知配置到数据库失败: %v", err)
			http.Error(w, "保存通知配置失败", http.StatusInternalServerError)
			return
		}

		log.Printf("通知配置保存成功: type=%s, enabled=%v", newConfig.Type, newConfig.Enabled)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Type       string               `json:"type"`
		WebhookURL string               `json:"webhook_url"`
		Secret     string               `json:"secret"`
		Email      notifier.EmailConfig `json:"email"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// 按类型校验必填项
	if req.Type == "email" {
		if req.Email.SMTPHost == "" || req.Email.From == "" || req.Email.To == "" {
			http.Error(w, "请填写 SMTP 服务器、发件人和收件人", http.StatusBadRequest)
			return
		}
	} else if req.WebhookURL == "" {
		http.Error(w, "Webhook URL不能为空", http.StatusBadRequest)
		return
	}

	config := notifier.NotificationConfig{
		Type:       req.Type,
		WebhookURL: req.WebhookURL,
		Secret:     req.Secret,
		Email:      req.Email,
		Enabled:    true,
	}

	err := s.notifier.SendTestNotification(config)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "测试通知发送成功",
	})
}

func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[通知API] 解析请求失败: %v", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	log.Printf("[通知API] 收到通知请求，类型: %s", req.Type)

	// 获取当前检测状态以获取信号信息
	status := s.detector.GetStatus()
	signals := status.Signals

	// 转换为 detector.NotifierSignal 接口
	notifierSignals := make([]detector.NotifierSignal, len(signals))
	for i, sig := range signals {
		notifierSignals[i] = notifierSignal{name: sig.Name}
	}

	switch req.Type {
	case "remote_start":
		log.Printf("[通知API] 处理远程开始通知")
		s.notifier.NotifyRemoteStart(notifierSignals)
	case "remote_end":
		log.Printf("[通知API] 处理远程结束通知")
		s.notifier.NotifyRemoteEnd(notifierSignals)
	case "app_exit":
		log.Printf("[通知API] 处理程序退出通知")
		s.notifier.NotifyAppExit()
		log.Printf("[通知API] 程序退出通知已发送")
	default:
		log.Printf("[通知API] 未知的通知类型: %s", req.Type)
		http.Error(w, "Unknown notification type", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	response := map[string]interface{}{
		"success": true,
		"message": "退出通知已发送",
	}
	log.Printf("[通知API] 返回响应: %+v", response)
	json.NewEncoder(w).Encode(response)
}

// notifierSignal 实现 detector.NotifierSignal 接口
type notifierSignal struct {
	name string
}

func (n notifierSignal) GetName() string {
	return n.name
}

func (s *Server) handleDeviceName(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		name, _ := s.storage.GetConfig("device_name")
		hostname, _ := os.Hostname()
		json.NewEncoder(w).Encode(map[string]string{
			"name":     name,
			"hostname": hostname,
		})

	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if err := s.storage.SetConfig("device_name", req.Name); err != nil {
			log.Printf("[设备名] 保存失败: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "保存失败"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// rulesUpdateBaseURL 读取规则更新源地址（配置项 rules_update_url，缺省用默认）。
func (s *Server) rulesUpdateBaseURL() string {
	if url, _ := s.storage.GetConfig("rules_update_url"); url != "" {
		return url
	}
	return defaultRulesUpdateURL
}

// writeJSONError 按项目约定输出 {"success":false,"error":"中文"} + 指定状态码。
func writeJSONError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   msg,
	})
}

// handleRulesVersion 返回当前规则版本、主程序版本及全部历史版本（供展示与回滚）。
func (s *Server) handleRulesVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	currentVersion, err := s.detector.GetActiveRuleVersion()
	if err != nil {
		writeJSONError(w, "获取当前规则版本失败", http.StatusInternalServerError)
		return
	}
	ruleSets, err := s.detector.ListRuleVersions()
	if err != nil {
		writeJSONError(w, "获取规则版本列表失败", http.StatusInternalServerError)
		return
	}

	versions := make([]map[string]interface{}, 0, len(ruleSets))
	currentSource := ""
	for _, rs := range ruleSets {
		versions = append(versions, map[string]interface{}{
			"version":       rs.Version,
			"minAppVersion": rs.MinAppVersion,
			"source":        rs.Source,
			"active":        rs.Active,
			"createdAt":     rs.CreatedAt.Format(time.RFC3339),
		})
		if rs.Active {
			currentSource = rs.Source
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"currentVersion": currentVersion,
		"appVersion":     version.Version,
		"source":         currentSource,
		"versions":       versions,
	})
}

// handleRulesCheck 检查 GitHub 上是否有新版本规则（不应用）。
// 若当前主程序版本低于规则要求的 minAppVersion，返回 requiresAppUpgrade=true。
func (s *Server) handleRulesCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	currentVersion, _ := s.detector.GetActiveRuleVersion()

	info, err := ruleupdate.FetchVersion(s.rulesUpdateBaseURL())
	if err != nil {
		log.Printf("[规则更新] 检查更新失败: %v", err)
		writeJSONError(w, "检查更新失败，请稍后重试: "+err.Error(), http.StatusInternalServerError)
		return
	}

	updateAvailable := ruleupdate.IsNewer(info.Version, currentVersion)
	requiresAppUpgrade := updateAvailable && info.MinAppVersion != "" &&
		ruleupdate.CompareVersions(version.Version, info.MinAppVersion) < 0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"updateAvailable":    updateAvailable,
		"currentVersion":     currentVersion,
		"latestVersion":      info.Version,
		"description":        info.Description,
		"appVersion":         version.Version,
		"requiresAppUpgrade": requiresAppUpgrade,
		"requiredAppVersion": info.MinAppVersion,
	})
}

// handleRulesApply 下载并应用 GitHub 上的最新规则。若主程序版本过低则拒绝。
func (s *Server) handleRulesApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ruleVersion, minAppVersion, rulesJSON, err := ruleupdate.FetchRules(s.rulesUpdateBaseURL())
	if err != nil {
		log.Printf("[规则更新] 下载规则失败: %v", err)
		writeJSONError(w, "下载规则失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 版本门槛：当前主程序版本不能低于规则要求的最低版本
	if minAppVersion != "" && ruleupdate.CompareVersions(version.Version, minAppVersion) < 0 {
		writeJSONError(w, "当前主程序版本过低，请先升级主程序到 v"+minAppVersion+" 后再升级规则", http.StatusBadRequest)
		return
	}

	// 校验规则 JSON 可被正确解析
	if _, err := detector.ParseRules(rulesJSON); err != nil {
		writeJSONError(w, "规则格式无效: "+err.Error(), http.StatusBadRequest)
		return
	}

	ruleSet, err := s.storage.SaveRuleSet(ruleVersion, minAppVersion, rulesJSON, "github")
	if err != nil {
		writeJSONError(w, "保存规则失败", http.StatusInternalServerError)
		return
	}
	if err := s.storage.SetActiveRuleSet(ruleSet.ID); err != nil {
		writeJSONError(w, "应用规则失败", http.StatusInternalServerError)
		return
	}
	if err := s.detector.ReloadRules(); err != nil {
		writeJSONError(w, "重载规则失败", http.StatusInternalServerError)
		return
	}

	log.Printf("[规则更新] 已应用规则 v%s", ruleVersion)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "规则已更新到 v" + ruleVersion,
		"version": ruleVersion,
	})
}

// handleRulesUpload 手工导入规则（面向内网/离线环境）：直接上传一份 rules.json 内容并应用。
// 与自动更新共用同一份规则格式与版本门槛校验，来源标记为 "manual"。
func (s *Server) handleRulesUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, "读取上传内容失败", http.StatusBadRequest)
		return
	}

	ruleVersion, minAppVersion, rulesJSON, err := ruleupdate.ParseRulesContent(body)
	if err != nil {
		writeJSONError(w, "规则文件解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 版本门槛：当前主程序版本不能低于规则要求的最低版本
	if minAppVersion != "" && ruleupdate.CompareVersions(version.Version, minAppVersion) < 0 {
		writeJSONError(w, "当前主程序版本过低，请先升级主程序到 v"+minAppVersion+" 后再导入规则", http.StatusBadRequest)
		return
	}

	// 校验规则 JSON 可被正确解析
	if _, err := detector.ParseRules(rulesJSON); err != nil {
		writeJSONError(w, "规则格式无效: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 手工导入要求版本号唯一：版本号即"内容指纹"，避免覆盖既有版本或与历史混淆
	if existing, _ := s.storage.GetRuleSetByVersion(ruleVersion); existing != nil {
		writeJSONError(w, "规则版本 v"+ruleVersion+" 已存在，请修改文件中的 version 字段后重试", http.StatusBadRequest)
		return
	}

	ruleSet, err := s.storage.SaveRuleSet(ruleVersion, minAppVersion, rulesJSON, "manual")
	if err != nil {
		writeJSONError(w, "保存规则失败", http.StatusInternalServerError)
		return
	}
	if err := s.storage.SetActiveRuleSet(ruleSet.ID); err != nil {
		writeJSONError(w, "应用规则失败", http.StatusInternalServerError)
		return
	}
	if err := s.detector.ReloadRules(); err != nil {
		writeJSONError(w, "重载规则失败", http.StatusInternalServerError)
		return
	}

	log.Printf("[规则更新] 已手工导入规则 v%s", ruleVersion)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "已导入规则 v" + ruleVersion,
		"version": ruleVersion,
	})
}

// handleRulesRollback 回滚到指定的历史规则版本。
func (s *Server) handleRulesRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Version == "" {
		writeJSONError(w, "请指定要回滚的版本", http.StatusBadRequest)
		return
	}

	ruleSet, err := s.storage.GetRuleSetByVersion(req.Version)
	if err != nil {
		writeJSONError(w, "查询规则版本失败", http.StatusInternalServerError)
		return
	}
	if ruleSet == nil {
		writeJSONError(w, "未找到规则版本 v"+req.Version, http.StatusBadRequest)
		return
	}

	if err := s.storage.SetActiveRuleSet(ruleSet.ID); err != nil {
		writeJSONError(w, "回滚失败", http.StatusInternalServerError)
		return
	}
	if err := s.detector.ReloadRules(); err != nil {
		writeJSONError(w, "重载规则失败", http.StatusInternalServerError)
		return
	}

	log.Printf("[规则更新] 已回滚到规则 v%s", req.Version)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "已回滚到 v" + req.Version,
		"version": req.Version,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := map[string]interface{}{
		"running": s.running,
		"port":    s.port,
		"time":    time.Now().Format(time.RFC3339),
	}
	json.NewEncoder(w).Encode(status)
}
