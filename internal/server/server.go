package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"RemoteKnown/internal/detector"
	"RemoteKnown/internal/notifier"
	"RemoteKnown/internal/storage"
)

const (
	DefaultPort = 18080
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
			Enabled    bool   `json:"enabled"`
			Type       string `json:"type"`
			WebhookURL string `json:"webhook_url"`
			Secret     string `json:"secret"`
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

		// 更新对应类型的具体配置
		typeConfig := map[string]interface{}{
			"webhook_url": newConfig.WebhookURL,
			"secret":      newConfig.Secret,
		}
		allConfigs[newConfig.Type] = typeConfig

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
		Type       string `json:"type"`
		WebhookURL string `json:"webhook_url"`
		Secret     string `json:"secret"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.WebhookURL == "" {
		http.Error(w, "Webhook URL不能为空", http.StatusBadRequest)
		return
	}

	config := notifier.NotificationConfig{
		Type:       req.Type,
		WebhookURL: req.WebhookURL,
		Secret:     req.Secret,
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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := map[string]interface{}{
		"running": s.running,
		"port":    s.port,
		"time":    time.Now().Format(time.RFC3339),
	}
	json.NewEncoder(w).Encode(status)
}
