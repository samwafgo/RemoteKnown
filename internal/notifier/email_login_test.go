package notifier

import (
	"bufio"
	"encoding/base64"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"testing"
)

// fakeSMTPResult 记录假 SMTP 服务器观察到的客户端行为。
type fakeSMTPResult struct {
	mu              sync.Mutex
	triedPlainAuth  bool
	loginSucceeded  bool
	messageReceived bool
}

// startLoginOnlySMTP 启动一个模拟内网服务器的假 SMTP：明文、EHLO 仅通告 AUTH LOGIN，
// 对 AUTH PLAIN 回 504（复现用户遇到的 "504 Authentication mechanism not supported"），
// 正常处理 AUTH LOGIN 流程与 MAIL/RCPT/DATA。返回监听地址端口与结果指针。
func startLoginOnlySMTP(t *testing.T) (port string, res *fakeSMTPResult) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	res = &fakeSMTPResult{}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer ln.Close()
		r := bufio.NewReader(conn)
		w := func(s string) { conn.Write([]byte(s + "\r\n")) }

		w("220 fake-smtp ready")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimSpace(line)
			upper := strings.ToUpper(cmd)
			switch {
			case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
				// 仅通告 LOGIN（不含 PLAIN）
				w("250-fake-smtp")
				w("250 AUTH LOGIN")
			case strings.HasPrefix(upper, "AUTH PLAIN"):
				res.mu.Lock()
				res.triedPlainAuth = true
				res.mu.Unlock()
				w("504 Authentication mechanism not supported")
			case strings.HasPrefix(upper, "AUTH LOGIN"):
				w("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
				if _, err := r.ReadString('\n'); err != nil { // 用户名(base64)
					return
				}
				w("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
				if _, err := r.ReadString('\n'); err != nil { // 密码(base64)
					return
				}
				res.mu.Lock()
				res.loginSucceeded = true
				res.mu.Unlock()
				w("235 2.7.0 Authentication successful")
			case strings.HasPrefix(upper, "MAIL FROM"):
				w("250 OK")
			case strings.HasPrefix(upper, "RCPT TO"):
				w("250 OK")
			case upper == "DATA":
				w("354 End data with <CR><LF>.<CR><LF>")
				for {
					dl, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimSpace(dl) == "." {
						break
					}
				}
				res.mu.Lock()
				res.messageReceived = true
				res.mu.Unlock()
				w("250 OK queued")
			case upper == "QUIT":
				w("221 Bye")
				return
			case upper == "RSET", upper == "NOOP":
				w("250 OK")
			default:
				w("250 OK")
			}
		}
	}()

	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, res
}

// TestSendEmailLoginOnlyServer 复现并验证内网修复：服务器只支持 AUTH LOGIN 时，
// 不应再强行 AUTH PLAIN（504），而应走 LOGIN 成功投递。
func TestSendEmailLoginOnlyServer(t *testing.T) {
	port, res := startLoginOnlySMTP(t)

	portNum := 0
	for _, c := range port {
		portNum = portNum*10 + int(c-'0')
	}

	n := &Notifier{}
	cfg := NotificationConfig{
		Type:    "email",
		Enabled: true,
		Email: EmailConfig{
			SMTPHost:   "127.0.0.1",
			SMTPPort:   portNum,
			Encryption: "none",
			Username:   "admin@Email.com",
			Password:   "secret",
			From:       "admin@Email.com",
			To:         "admin@Email.com",
		},
	}

	if err := n.sendEmailNotification(cfg, "测试标题", "测试内容"); err != nil {
		t.Fatalf("内网 LOGIN-only 服务器发送应成功，但失败: %v", err)
	}

	res.mu.Lock()
	defer res.mu.Unlock()
	if res.triedPlainAuth {
		t.Errorf("不应再对只支持 LOGIN 的服务器尝试 AUTH PLAIN")
	}
	if !res.loginSucceeded {
		t.Errorf("应通过 AUTH LOGIN 完成认证")
	}
	if !res.messageReceived {
		t.Errorf("服务器应收到完整邮件数据")
	}
}

// TestChooseSMTPAuth 校验按服务器通告的机制挑选认证方式（覆盖 hMailServer 的各种通告组合）。
func TestChooseSMTPAuth(t *testing.T) {
	e := EmailConfig{Username: "u", Password: "p", SMTPHost: "h"}
	info := &smtp.ServerInfo{Name: "h", TLS: true, Auth: []string{"PLAIN", "LOGIN", "CRAM-MD5"}}
	mechOf := func(a smtp.Auth) string {
		if a == nil {
			return ""
		}
		m, _, _ := a.Start(info)
		return m
	}

	cases := []struct {
		mechs string
		isTLS bool
		want  string
	}{
		{"LOGIN", false, "LOGIN"},          // hMailServer 仅通告 LOGIN
		{"CRAM-MD5", false, "CRAM-MD5"},    // hMailServer 关闭明文认证时仅通告 CRAM-MD5
		{"CRAM-MD5 LOGIN", false, "LOGIN"}, // 同时通告：优先可用的明文 LOGIN
		{"PLAIN", false, "PLAIN"},          // 明文 PLAIN（自定义 noTLS 实现）
		{"PLAIN", true, "PLAIN"},           // 加密 PLAIN（标准库）
		{"LOGIN PLAIN", false, "PLAIN"},    // PLAIN 优先于 LOGIN
		{"", false, ""},                    // 未通告 AUTH → 无认证（内网匿名）
		{"NTLM GSSAPI", false, ""},         // 仅不支持的机制 → 无可用认证
	}
	for _, c := range cases {
		got := mechOf(chooseSMTPAuth(c.mechs, e, c.isTLS))
		if got != c.want {
			t.Errorf("chooseSMTPAuth(%q, isTLS=%v) 选中机制=%q, 期望=%q", c.mechs, c.isTLS, got, c.want)
		}
	}
}
