package notifier

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestParseRecipients(t *testing.T) {
	cases := map[string]int{
		"":                          0,
		"a@x.com":                   1,
		"a@x.com,b@x.com":           2,
		"a@x.com; b@x.com  c@x.com": 3,
		" a@x.com , , b@x.com ":     2,
	}
	for in, want := range cases {
		if got := len(parseRecipients(in)); got != want {
			t.Errorf("parseRecipients(%q) = %d, want %d", in, got, want)
		}
	}
}

// fakeSMTPServer 启动一个最小可用的明文 SMTP 服务器，捕获收到的信封与正文。
type smtpCapture struct {
	mailFrom string
	rcpts    []string
	data     string
}

func startFakeSMTP(t *testing.T) (host string, port int, result <-chan smtpCapture) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	ch := make(chan smtpCapture, 1)

	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		r := bufio.NewReader(conn)
		write := func(s string) { conn.Write([]byte(s + "\r\n")) }

		write("220 fake ESMTP ready")
		var cap smtpCapture
		var dataBuf strings.Builder
		inData := false

		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					cap.data = dataBuf.String()
					write("250 OK queued")
					continue
				}
				dataBuf.WriteString(line)
				continue
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				write("250-fake greets you")
				write("250 OK")
			case strings.HasPrefix(cmd, "MAIL FROM"):
				cap.mailFrom = strings.TrimSpace(line)
				write("250 OK")
			case strings.HasPrefix(cmd, "RCPT TO"):
				cap.rcpts = append(cap.rcpts, strings.TrimSpace(line))
				write("250 OK")
			case cmd == "DATA":
				write("354 End data with <CR><LF>.<CR><LF>")
				inData = true
			case cmd == "QUIT":
				write("221 Bye")
				ch <- cap
				return
			default:
				write("250 OK")
			}
		}
		ch <- cap
	}()

	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(p)
	return h, port, ch
}

// TestSendEmailPlaintextNoAuth 验证内网场景：不加密、无认证的明文 SMTP 发送。
func TestSendEmailPlaintextNoAuth(t *testing.T) {
	host, port, result := startFakeSMTP(t)

	n := &Notifier{}
	cfg := NotificationConfig{
		Type: "email",
		Email: EmailConfig{
			SMTPHost:   host,
			SMTPPort:   port,
			Encryption: "none",
			From:       "alert@example.com",
			To:         "a@example.com, b@example.com",
		},
	}

	if err := n.sendEmailNotification(cfg, "远程控制告警", "检测到远程会话"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	got := <-result
	if !strings.Contains(got.mailFrom, "alert@example.com") {
		t.Errorf("MAIL FROM 未包含发件人: %q", got.mailFrom)
	}
	if len(got.rcpts) != 2 {
		t.Fatalf("收件人数量应为 2, 实际 %d: %v", len(got.rcpts), got.rcpts)
	}
	if !strings.Contains(got.data, "Subject:") {
		t.Errorf("邮件缺少 Subject 头:\n%s", got.data)
	}
	if !strings.Contains(got.data, "Content-Transfer-Encoding: base64") {
		t.Errorf("邮件正文未使用 base64 编码:\n%s", got.data)
	}
}
