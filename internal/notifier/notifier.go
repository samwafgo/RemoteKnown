package notifier

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/smtp"
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
	Enabled    bool        `json:"enabled"`
	Type       string      `json:"type"`        // "feishu" / "dingtalk" / "email"
	WebhookURL string      `json:"webhook_url"` // Webhook URL（飞书/钉钉）
	Secret     string      `json:"secret"`      // 签名密钥（可选，飞书/钉钉）
	Email      EmailConfig `json:"email"`       // 邮件配置（Type == "email" 时使用）
}

// EmailConfig SMTP 邮件通知配置
type EmailConfig struct {
	SMTPHost   string `json:"smtp_host"`  // SMTP 服务器地址
	SMTPPort   int    `json:"smtp_port"`  // SMTP 端口；0 表示按加密方式取默认端口
	Encryption string `json:"encryption"` // 加密方式："none"(明文/内网) / "starttls" / "ssl"
	Username   string `json:"username"`   // 认证用户名；留空表示匿名发送（内网常见）
	Password   string `json:"password"`   // 认证密码 / 授权码
	From       string `json:"from"`       // 发件人地址
	To         string `json:"to"`         // 收件人地址，多个用逗号/分号/空格分隔
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
	if currentType == "email" {
		// 邮件配置：借助 json 标签直接反序列化到 EmailConfig
		if raw, ok := allConfigs["email"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				json.Unmarshal(b, &config.Email)
			}
		}
	} else if typeConfig, ok := allConfigs[currentType].(map[string]interface{}); ok {
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
	case "email":
		return n.sendEmailNotification(config, title, content)
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

// sendEmailNotification 通过 SMTP 发送邮件通知
func (n *Notifier) sendEmailNotification(config NotificationConfig, title, content string) error {
	e := config.Email
	if e.SMTPHost == "" {
		return fmt.Errorf("SMTP 服务器地址不能为空")
	}
	if e.From == "" {
		return fmt.Errorf("发件人地址不能为空")
	}

	recipients := parseRecipients(e.To)
	if len(recipients) == 0 {
		return fmt.Errorf("收件人地址不能为空")
	}

	// 端口为 0 时按加密方式取默认端口
	port := e.SMTPPort
	if port == 0 {
		switch strings.ToLower(e.Encryption) {
		case "ssl":
			port = 465
		case "starttls":
			port = 587
		default:
			port = 25
		}
	}
	addr := net.JoinHostPort(e.SMTPHost, fmt.Sprintf("%d", port))

	msg := buildEmailMessage(e.From, recipients, title, content)

	if debugMode {
		log.Printf("[通知器] 发送邮件: addr=%s 加密=%s 收件人=%v 认证=%v",
			addr, e.Encryption, recipients, e.Username != "")
	}

	client, err := dialSMTP(addr, e)
	if err != nil {
		return err
	}
	defer client.Close()

	// STARTTLS：明文连接后升级到 TLS
	if strings.EqualFold(e.Encryption, "starttls") {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("服务器不支持 STARTTLS，请改用“不加密”或“SSL”")
		}
		if err := client.StartTLS(&tls.Config{ServerName: e.SMTPHost}); err != nil {
			return fmt.Errorf("STARTTLS 升级失败: %w", err)
		}
	}

	// 认证（可选）：填了用户名才认证，内网匿名发送可留空
	authenticated := false
	if e.Username != "" {
		if ok, _ := client.Extension("AUTH"); ok {
			var auth smtp.Auth
			if _, isTLS := client.TLSConnectionState(); isTLS {
				auth = smtp.PlainAuth("", e.Username, e.Password, e.SMTPHost)
			} else {
				// 明文连接下标准库 PlainAuth 会拒绝发送凭据，使用自定义实现以支持内网明文认证
				auth = &plainAuthNoTLS{username: e.Username, password: e.Password}
			}
			if err := client.Auth(auth); err != nil {
				return fmt.Errorf("SMTP 认证失败（请检查用户名/密码；QQ/163/Gmail 等公网邮箱密码必须用“授权码”而非登录密码）: %w", err)
			}
			authenticated = true
		} else {
			return fmt.Errorf("服务器未提供 AUTH 认证，通常是因为连接未加密。公网邮箱请把“加密方式”改为 SSL(465) 或 STARTTLS(587) 后重试")
		}
	}

	if err := client.Mail(e.From); err != nil {
		// 多数公网邮箱要求先登录才能发信（如 503 need AUTH first）
		if !authenticated {
			return fmt.Errorf("设置发件人失败，服务器很可能要求先登录认证。请填写用户名/密码，并将加密方式选为 SSL 或 STARTTLS（公网邮箱密码用“授权码”）: %w", err)
		}
		return fmt.Errorf("设置发件人失败: %w", err)
	}
	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("设置收件人 %s 失败: %w", rcpt, err)
		}
	}

	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("准备发送邮件数据失败: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		wc.Close()
		return fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("发送邮件失败: %w", err)
	}

	return client.Quit()
}

// dialSMTP 根据加密方式建立 SMTP 连接：ssl 走隐式 TLS，其余先明文连接（starttls 稍后升级）
func dialSMTP(addr string, e EmailConfig) (*smtp.Client, error) {
	if strings.EqualFold(e.Encryption, "ssl") {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{ServerName: e.SMTPHost})
		if err != nil {
			return nil, fmt.Errorf("SSL 连接 SMTP 服务器失败: %w", err)
		}
		client, err := smtp.NewClient(conn, e.SMTPHost)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("创建 SMTP 客户端失败: %w", err)
		}
		return client, nil
	}

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	client, err := smtp.NewClient(conn, e.SMTPHost)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("创建 SMTP 客户端失败: %w", err)
	}
	return client, nil
}

// buildEmailMessage 构建符合 RFC 822 的纯文本邮件（主题/正文按 UTF-8 编码，避免中文乱码）
func buildEmailMessage(from string, to []string, subject, body string) []byte {
	var buf bytes.Buffer
	buf.WriteString("From: " + from + "\r\n")
	buf.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	buf.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	buf.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	buf.WriteString("Content-Transfer-Encoding: base64\r\n")
	buf.WriteString("\r\n")

	// 正文用 base64 编码，按 RFC 2045 每 76 字符换行
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		buf.WriteString(encoded[i:end] + "\r\n")
	}
	return buf.Bytes()
}

// parseRecipients 解析收件人列表，支持逗号/分号/空白分隔
func parseRecipients(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	var out []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// plainAuthNoTLS 是不校验 TLS 的 PLAIN 认证实现。
// 标准库 smtp.PlainAuth 会拒绝在明文（非 TLS、非 localhost）连接上发送凭据，
// 内网无加密 SMTP 服务器需要认证时用它绕过该限制。
type plainAuthNoTLS struct {
	username, password string
}

func (a *plainAuthNoTLS) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (a *plainAuthNoTLS) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("意外的服务器认证质询")
	}
	return nil, nil
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
