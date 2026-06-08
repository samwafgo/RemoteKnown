package notifier

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"RemoteKnown/internal/detector"
	"RemoteKnown/internal/storage"
)

var debugMode = os.Getenv("REMOTEKNOWN_DEBUG") != ""

// NotificationConfig 通知配置
type NotificationConfig struct {
	Enabled    bool   `json:"enabled"`
	Type       string `json:"type"`        // "feishu" 或 "dingtalk"
	WebhookURL string `json:"webhook_url"` // Webhook URL
	Secret     string `json:"secret"`      // 签名密钥（可选）
}

// Notifier 通知器
type Notifier struct {
	storage *storage.Storage
}

// NewNotifier 创建新的通知器
func NewNotifier(storage *storage.Storage) *Notifier {
	return &Notifier{
		storage: storage,
	}
}

// NotifyRemoteStart 通知远程控制开始
func (n *Notifier) NotifyRemoteStart(signals []detector.NotifierSignal) {
	config, err := n.getConfig()
	if err != nil || !config.Enabled {
		log.Printf("[通知器] 通知未启用或配置读取失败")
		return
	}

	signalNames := make([]string, len(signals))
	for i, sig := range signals {
		signalNames[i] = sig.GetName()
	}

	title := "⚠️ 远程控制检测告警"
	content := fmt.Sprintf("主机：%s\n\n检测到远程控制连接已建立\n\n检测信号：\n%s\n\n时间：%s",
		n.getDeviceName(),
		strings.Join(signalNames, "\n"),
		time.Now().Format("2006-01-02 15:04:05"))

	if err := n.sendNotification(config, title, content); err != nil {
		log.Printf("[通知器] 发送远程开始通知失败: %v", err)
	} else {
		log.Printf("[通知器] 远程开始通知已发送")
	}
}

// NotifyRemoteEnd 通知远程控制结束
func (n *Notifier) NotifyRemoteEnd(signals []detector.NotifierSignal) {
	config, err := n.getConfig()
	if err != nil || !config.Enabled {
		log.Printf("[通知器] 通知未启用或配置读取失败")
		return
	}

	signalNames := make([]string, len(signals))
	for i, sig := range signals {
		signalNames[i] = sig.GetName()
	}

	title := "✅ 远程控制已断开"
	content := fmt.Sprintf("主机：%s\n\n远程控制会话已结束\n\n上次检测信号：\n%s\n\n时间：%s",
		n.getDeviceName(),
		strings.Join(signalNames, "\n"),
		time.Now().Format("2006-01-02 15:04:05"))

	if err := n.sendNotification(config, title, content); err != nil {
		log.Printf("[通知器] 发送远程结束通知失败: %v", err)
	} else {
		log.Printf("[通知器] 远程结束通知已发送")
	}
}

// NotifyAppExit 通知应用退出
func (n *Notifier) NotifyAppExit() {
	config, err := n.getConfig()
	if err != nil || !config.Enabled {
		log.Printf("[通知器] 通知未启用或配置读取失败")
		return
	}

	title := "🔴 RemoteKnown 服务已退出"
	content := fmt.Sprintf("主机：%s\n\nRemoteKnown 守护进程已停止运行\n\n时间：%s",
		n.getDeviceName(),
		time.Now().Format("2006-01-02 15:04:05"))

	if err := n.sendNotification(config, title, content); err != nil {
		log.Printf("[通知器] 发送应用退出通知失败: %v", err)
	} else {
		log.Printf("[通知器] 应用退出通知已发送")
	}
}

// SendTestNotification 发送测试通知
func (n *Notifier) SendTestNotification(config NotificationConfig) error {
	title := "🧪 测试通知"
	content := fmt.Sprintf("主机：%s\n\n这是一条测试通知\n\n通知类型：%s\n发送时间：%s",
		n.getDeviceName(),
		config.Type,
		time.Now().Format("2006-01-02 15:04:05"))

	return n.sendNotification(config, title, content)
}

// getDeviceName 获取设备标识名，优先用用户配置，未配置则返回主机名
func (n *Notifier) getDeviceName() string {
	name, err := n.storage.GetConfig("device_name")
	if err == nil && name != "" {
		return name
	}
	if hostname, err := os.Hostname(); err == nil {
		return hostname
	}
	return "未知主机"
}

// getConfig 从存储中获取通知配置
func (n *Notifier) getConfig() (NotificationConfig, error) {
	var config NotificationConfig

	// 尝试读取新格式的配置
	allConfigJSON, err := n.storage.GetConfig("notification_configs")
	if err != nil {
		return config, err
	}

	// 如果新格式不存在，尝试读取旧格式
	if allConfigJSON == "" {
		oldConfigJSON, err := n.storage.GetConfig("notification_config")
		if err != nil || oldConfigJSON == "" {
			return config, fmt.Errorf("未找到通知配置")
		}

		// 解析旧格式配置
		var oldConfig map[string]interface{}
		if err := json.Unmarshal([]byte(oldConfigJSON), &oldConfig); err != nil {
			return config, err
		}

		// 从旧格式读取配置
		if enabled, ok := oldConfig["enabled"].(bool); ok {
			config.Enabled = enabled
		}
		if t, ok := oldConfig["type"].(string); ok {
			config.Type = t
		}
		if webhookURL, ok := oldConfig["webhook_url"].(string); ok {
			config.WebhookURL = webhookURL
		}
		if secret, ok := oldConfig["secret"].(string); ok {
			config.Secret = secret
		}

		return config, nil
	}

	// 解析新格式的完整配置
	var allConfigs map[string]interface{}
	if err := json.Unmarshal([]byte(allConfigJSON), &allConfigs); err != nil {
		return config, err
	}

	// 获取基本配置
	if enabled, ok := allConfigs["enabled"].(bool); ok {
		config.Enabled = enabled
	}

	currentType := "feishu"
	if t, ok := allConfigs["type"].(string); ok {
		currentType = t
		config.Type = currentType
	}

	// 获取对应类型的具体配置
	if typeConfig, ok := allConfigs[currentType].(map[string]interface{}); ok {
		if webhookURL, ok := typeConfig["webhook_url"].(string); ok {
			config.WebhookURL = webhookURL
		}
		if secret, ok := typeConfig["secret"].(string); ok {
			config.Secret = secret
		}
	}

	return config, nil
}

// sendNotification 发送通知
func (n *Notifier) sendNotification(config NotificationConfig, title, content string) error {
	switch config.Type {
	case "feishu":
		return n.sendFeishuNotification(config, title, content)
	case "dingtalk":
		return n.sendDingtalkNotification(config, title, content)
	default:
		return fmt.Errorf("不支持的通知类型: %s", config.Type)
	}
}

// sendFeishuNotification 发送飞书通知
func (n *Notifier) sendFeishuNotification(config NotificationConfig, title, content string) error {
	// 构建飞书消息体
	message := map[string]interface{}{
		"msg_type": "interactive",
		"card": map[string]interface{}{
			"header": map[string]interface{}{
				"title": map[string]interface{}{
					"tag":     "plain_text",
					"content": title,
				},
				"template": getFeishuColor(title),
			},
			"elements": []map[string]interface{}{
				{
					"tag": "div",
					"text": map[string]interface{}{
						"tag":     "plain_text",
						"content": content,
					},
				},
			},
		},
	}

	// 如果配置了签名密钥，添加签名
	if config.Secret != "" {
		timestamp := time.Now().Unix()
		sign := generateFeishuSign(config.Secret, timestamp)
		message["timestamp"] = fmt.Sprintf("%d", timestamp)
		message["sign"] = sign
	}

	if debugMode {
		if raw, err := json.Marshal(message); err == nil {
			log.Printf("[通知器] 飞书消息原始内容: %s", string(raw))
		}
	}
	return n.postWebhook(config.WebhookURL, message)
}

// sendDingtalkNotification 发送钉钉通知
func (n *Notifier) sendDingtalkNotification(config NotificationConfig, title, content string) error {
	message := map[string]interface{}{
		"msgtype": "markdown",
		"markdown": map[string]interface{}{
			"title": title,
			"text":  fmt.Sprintf("### %s\n\n%s", title, content),
		},
	}

	webhookURL := config.WebhookURL

	// 如果配置了签名密钥，将签名和时间戳附加到URL
	if config.Secret != "" {
		timestamp := time.Now().UnixMilli()
		sign := generateDingtalkSign(config.Secret, timestamp)
		webhookURL = fmt.Sprintf("%s&timestamp=%d&sign=%s", config.WebhookURL, timestamp, url.QueryEscape(sign))
	}

	return n.postWebhook(webhookURL, message)
}

// postWebhook 发送 webhook 请求
func (n *Notifier) postWebhook(url string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("序列化消息失败: %w", err)
	}

	log.Printf("[通知器] 发送 Webhook 请求到: %s", url)

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("发送 HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 请求失败，状态码: %d", resp.StatusCode)
	}

	// 读取响应
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[通知器] 解析响应失败: %v", err)
		return nil // 忽略解析错误，因为通知可能已经发送成功
	}

	log.Printf("[通知器] Webhook 响应: %+v", result)

	// 检查飞书响应
	if code, ok := result["code"].(float64); ok && code != 0 {
		return fmt.Errorf("通知发送失败: %v", result["msg"])
	}

	// 检查钉钉响应
	if errcode, ok := result["errcode"].(float64); ok && errcode != 0 {
		return fmt.Errorf("通知发送失败: %v", result["errmsg"])
	}

	return nil
}

// generateFeishuSign 生成飞书签名
func generateFeishuSign(secret string, timestamp int64) string {
	stringToSign := fmt.Sprintf("%d\n%s", timestamp, secret)
	h := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// generateDingtalkSign 生成钉钉签名
func generateDingtalkSign(secret string, timestamp int64) string {
	stringToSign := fmt.Sprintf("%d\n%s", timestamp, secret)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(stringToSign))
	signData := h.Sum(nil)
	return base64.StdEncoding.EncodeToString(signData)
}

// getFeishuColor 根据标题获取飞书卡片颜色
func getFeishuColor(title string) string {
	if strings.Contains(title, "⚠️") || strings.Contains(title, "告警") {
		return "red"
	} else if strings.Contains(title, "✅") {
		return "green"
	} else if strings.Contains(title, "🔴") {
		return "red"
	}
	return "blue"
}
