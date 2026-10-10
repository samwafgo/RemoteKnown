package notifier

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	pushPlusEndpoint      = "https://www.pushplus.plus/send"
	pushPlusTimeout       = 10 * time.Second
	pushPlusResponseLimit = 64 * 1024
)

// PushPlusConfig 使用 PushPlus v1.18 API 的 option 字段，而不是旧的 webhook 字段。
// 请求地址固定，不允许配置自定义 URL，以免通知内容和 token 被发往其他服务。
type PushPlusConfig struct {
	Token   string `json:"token"`
	Channel string `json:"channel"`
	Option  string `json:"option"`
	To      string `json:"to"`
}

// Normalized 修剪字段、补齐默认渠道，并清除该渠道不支持的路由参数。
// 不改变渠道大小写：API 渠道必须严格匹配支持列表。
func (c PushPlusConfig) Normalized() PushPlusConfig {
	c.Token = strings.TrimSpace(c.Token)
	c.Channel = strings.TrimSpace(c.Channel)
	c.Option = strings.TrimSpace(c.Option)
	c.To = strings.TrimSpace(c.To)
	if c.Channel == "" {
		c.Channel = "wechat"
	}
	switch c.Channel {
	case "wechat":
		c.Option = ""
	case "cp", "qq", "mail":
		// 这三个渠道支持 option 和 to；QQ 群配置的组合限制由服务端检查。
	case "webhook":
		c.To = ""
	default:
		c.Option = ""
		c.To = ""
	}
	return c
}

// Validate 检查发送必需的配置；停用通知时可以只规整配置而不调用校验。
func (c PushPlusConfig) Validate() error {
	c = c.Normalized()
	if c.Token == "" {
		return fmt.Errorf("PushPlus token 不能为空")
	}
	switch c.Channel {
	case "wechat", "app", "extension", "webhook", "clawbot", "cmcc", "qq", "cp", "mail", "sms", "voice":
	default:
		return fmt.Errorf("PushPlus 渠道不受支持，请选择有效渠道")
	}
	if (c.Channel == "cp" || c.Channel == "webhook") && c.Option == "" {
		return fmt.Errorf("PushPlus 企业微信和 Webhook 渠道必须填写 option 配置编码")
	}
	return nil
}

// sendPushPlusNotification 返回 nil 仅表示异步请求已被受理，不代表最终送达。
func (n *Notifier) sendPushPlusNotification(config PushPlusConfig, title, content string) error {
	config = config.Normalized()
	if err := config.Validate(); err != nil {
		return err
	}
	payload := struct {
		Token    string `json:"token"`
		Title    string `json:"title"`
		Content  string `json:"content"`
		Template string `json:"template"`
		Channel  string `json:"channel"`
		Option   string `json:"option,omitempty"`
		To       string `json:"to,omitempty"`
	}{config.Token, title, content, "txt", config.Channel, config.Option, config.To}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("PushPlus 消息编码失败")
	}
	req, err := http.NewRequest(http.MethodPost, pushPlusEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PushPlus 请求创建失败")
	}
	req.Header.Set("Content-Type", "application/json")

	// 复制注入的客户端，保证超时和禁止重定向不被测试注入/复用客户端绕过。
	// nil Transport 保持标准库行为，允许测试拦截 http.DefaultTransport。
	client := http.Client{}
	if n.pushPlusClient != nil {
		client = *n.pushPlusClient
	}
	if client.Timeout <= 0 || client.Timeout > pushPlusTimeout {
		client.Timeout = pushPlusTimeout
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		// 原始错误可能包含敏感请求参数，绝不回传或记录。
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return fmt.Errorf("PushPlus 请求超时，请检查网络后重试")
		}
		return fmt.Errorf("PushPlus 请求失败，请检查网络和 HTTPS 连接后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PushPlus HTTP 请求失败（状态码 %d），请检查服务状态后重试", resp.StatusCode)
	}

	response, err := io.ReadAll(io.LimitReader(resp.Body, pushPlusResponseLimit+1))
	if err != nil {
		return fmt.Errorf("PushPlus 响应读取失败，请检查网络后重试")
	}
	if len(response) > pushPlusResponseLimit {
		return fmt.Errorf("PushPlus 响应超过大小限制，请稍后重试")
	}
	var result struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(response, &result); err != nil || result.Code == nil {
		return fmt.Errorf("PushPlus 响应格式错误或缺少有效业务码，请检查服务状态")
	}
	if *result.Code != 200 {
		// 业务消息可能反射 token 或正文，仅返回整数业务码。
		return fmt.Errorf("PushPlus 未受理请求（业务码 %d），请检查 token、渠道配置及账户额度", *result.Code)
	}
	return nil
}
