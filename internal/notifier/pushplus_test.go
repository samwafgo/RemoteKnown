package notifier

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"RemoteKnown/internal/storage"
)

type pushPlusRoundTripper func(*http.Request) (*http.Response, error)

func (f pushPlusRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func pushPlusResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestPushPlusPayloadChannels(t *testing.T) {
	cases := []struct {
		channel, option, to string
	}{
		{"", "", "friend"},
		{"wechat", "", "friend"},
		{"app", "", ""},
		{"extension", "", ""},
		{"webhook", "route", ""},
		{"clawbot", "", ""},
		{"cmcc", "", ""},
		{"qq", "route", ""},
		{"qq", "route", "friend"},
		{"qq", "", "friend"},
		{"qq", "", ""},
		{"cp", "route", "userID"},
		{"cp", "route", ""},
		{"mail", "route", "friend"},
		{"mail", "route", ""},
		{"mail", "", "friend"},
		{"mail", "", ""},
		{"sms", "", ""},
		{"voice", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.channel+"_"+tc.option+"_"+tc.to, func(t *testing.T) {
			config := PushPlusConfig{Token: " secret-token ", Channel: " " + tc.channel + " ", Option: " ignored ", To: " ignored "}
			if tc.channel == "cp" || tc.channel == "qq" || tc.channel == "mail" || tc.channel == "webhook" {
				config.Option = " " + tc.option + " "
			}
			if tc.channel == "" || tc.channel == "wechat" || tc.channel == "cp" || tc.channel == "qq" || tc.channel == "mail" {
				config.To = " " + tc.to + " "
			}
			title := "测试\"标题\n<plain>"
			content := "换行\n正文\\路径 & <script>不是 HTML</script>"
			calls := 0
			n := &Notifier{pushPlusClient: &http.Client{Transport: pushPlusRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != "https://www.pushplus.plus/send" || r.Method != http.MethodPost {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Fatal("request must be JSON")
				}
				var got map[string]string
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				channel := tc.channel
				if channel == "" {
					channel = "wechat"
				}
				want := map[string]string{"token": "secret-token", "title": title, "content": content, "template": "txt", "channel": channel}
				if tc.option != "" {
					want["option"] = tc.option
				}
				if tc.to != "" {
					want["to"] = tc.to
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("wrong payload fields: got %#v, want %#v", got, want)
				}
				return pushPlusResponse(200, `{"code":200,"msg":"异步请求已受理"}`), nil
			})}}
			if err := n.sendNotification(NotificationConfig{Type: "pushplus", PushPlus: config, WebhookURL: "https://not-used.invalid", Secret: "not-used"}, title, content); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("got %d calls", calls)
			}
		})
	}
}

func TestPushPlusValidation(t *testing.T) {
	cases := []struct {
		name      string
		config    PushPlusConfig
		wantError bool
	}{
		{"default", PushPlusConfig{Token: " token "}, false},
		{"blank token", PushPlusConfig{Token: " \n\t"}, true},
		{"unknown", PushPlusConfig{Token: "token", Channel: "secret-token"}, true},
		{"case sensitive", PushPlusConfig{Token: "token", Channel: "WECHAT"}, true},
		{"old alias", PushPlusConfig{Token: "token", Channel: "qywx"}, true},
		{"cp missing option", PushPlusConfig{Token: "token", Channel: "cp", Option: " "}, true},
		{"webhook missing option", PushPlusConfig{Token: "token", Channel: "webhook", Option: "\n"}, true},
		{"qq option with friend target", PushPlusConfig{Token: "token", Channel: "qq", Option: "route", To: "friend"}, false},
		{"qq option", PushPlusConfig{Token: "token", Channel: "qq", Option: "route"}, false},
		{"qq to", PushPlusConfig{Token: "token", Channel: "qq", To: "friend"}, false},
		{"mail optional", PushPlusConfig{Token: "token", Channel: "mail"}, false},
		{"irrelevant fields", PushPlusConfig{Token: "token", Channel: "app", Option: "ignored", To: "ignored"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if (err != nil) != tc.wantError {
				t.Fatalf("validation got %v, wantError %v", err, tc.wantError)
			}
			if tc.wantError {
				calls := 0
				n := &Notifier{pushPlusClient: &http.Client{Transport: pushPlusRoundTripper(func(_ *http.Request) (*http.Response, error) {
					calls++
					return nil, errors.New("unexpected call")
				})}}
				if err := n.sendPushPlusNotification(tc.config, "title", "content"); err == nil || strings.Contains(err.Error(), "secret-token") {
					t.Fatalf("invalid config was accepted or disclosed: %v", err)
				}
				if calls != 0 {
					t.Fatal("invalid config reached HTTP transport")
				}
			}
		})
	}
}

func TestPushPlusResponseValidation(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantError string
	}{
		{"accepted", 200, `{"code":200}`, ""},
		{"business failure", 200, `{"code":401,"msg":"secret-token"}`, "业务码 401"},
		{"business zero", 200, `{"code":0}`, "业务码 0"},
		{"missing", 200, `{"msg":"secret-token"}`, "响应格式"},
		{"null", 200, `{"code":null}`, "响应格式"},
		{"string", 200, `{"code":"200"}`, "响应格式"},
		{"boolean", 200, `{"code":true}`, "响应格式"},
		{"decimal", 200, `{"code":200.0}`, "响应格式"},
		{"fraction", 200, `{"code":200.5}`, "响应格式"},
		{"integer overflow", 200, `{"code":999999999999999999999999}`, "响应格式"},
		{"malformed", 200, `secret-token`, "响应格式"},
		{"empty", 200, "", "响应格式"},
		{"multiple values", 200, `{"code":200}{"code":200}`, "响应格式"},
		{"array", 200, `[{"code":200}]`, "响应格式"},
		{"http failure", 500, `{"code":200,"msg":"secret-token"}`, "状态码 500"},
		{"http created", 201, `{"code":200}`, "状态码 201"},
		{"oversized", 200, `{"code":200,"msg":"` + strings.Repeat("x", pushPlusResponseLimit) + `"}`, "大小限制"},
		{"limit exact", 200, `{"code":200}` + strings.Repeat(" ", pushPlusResponseLimit-len(`{"code":200}`)), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &Notifier{pushPlusClient: &http.Client{Transport: pushPlusRoundTripper(func(_ *http.Request) (*http.Response, error) {
				return pushPlusResponse(tc.status, tc.body), nil
			})}}
			err := n.sendPushPlusNotification(PushPlusConfig{Token: "secret-token"}, "secret-title", "secret-content")
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("got %v, want %q", err, tc.wantError)
			}
			for _, secret := range []string{"secret-token", "secret-title", "secret-content"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked sensitive data: %v", err)
				}
			}
		})
	}
}

type pushPlusBrokenBody struct{}

func (pushPlusBrokenBody) Read([]byte) (int, error) { return 0, errors.New("secret-token") }
func (pushPlusBrokenBody) Close() error             { return nil }

func TestPushPlusTransportErrors(t *testing.T) {
	t.Run("network error", func(t *testing.T) {
		n := &Notifier{pushPlusClient: &http.Client{Transport: pushPlusRoundTripper(func(_ *http.Request) (*http.Response, error) {
			return nil, errors.New("secret-token secret-content")
		})}}
		err := n.sendPushPlusNotification(PushPlusConfig{Token: "secret-token"}, "title", "secret-content")
		if err == nil || strings.Contains(err.Error(), "secret-") || !strings.Contains(err.Error(), "网络") {
			t.Fatalf("unexpected network error: %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		n := &Notifier{pushPlusClient: &http.Client{Timeout: 20 * time.Millisecond, Transport: pushPlusRoundTripper(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) > pushPlusTimeout {
				t.Error("request must have a bounded timeout")
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}}
		err := n.sendPushPlusNotification(PushPlusConfig{Token: "secret-token"}, "title", "content")
		if err == nil || !strings.Contains(err.Error(), "超时") || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("unexpected timeout error: %v", err)
		}
	})
	t.Run("response read error", func(t *testing.T) {
		n := &Notifier{pushPlusClient: &http.Client{Transport: pushPlusRoundTripper(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: pushPlusBrokenBody{}}, nil
		})}}
		err := n.sendPushPlusNotification(PushPlusConfig{Token: "secret-token"}, "title", "content")
		if err == nil || !strings.Contains(err.Error(), "读取失败") || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("unexpected read error: %v", err)
		}
	})
}

func TestPushPlusRejectsRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			n := &Notifier{pushPlusClient: &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return nil }, Transport: pushPlusRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != pushPlusEndpoint {
					t.Fatal("token forwarded to redirect destination")
				}
				resp := pushPlusResponse(status, "secret-token")
				resp.Header.Set("Location", "https://example.invalid/token-collector")
				return resp, nil
			})}}
			err := n.sendPushPlusNotification(PushPlusConfig{Token: "secret-token"}, "title", "content")
			if err == nil || calls != 1 || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("redirect not safely refused: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestPushPlusDefaultClient(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = pushPlusRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > pushPlusTimeout {
			t.Fatal("default client does not enforce 10 second timeout")
		}
		if r.URL.String() != pushPlusEndpoint {
			t.Fatal("wrong endpoint")
		}
		return pushPlusResponse(200, `{"code":200}`), nil
	})
	n := &Notifier{}
	if err := n.sendPushPlusNotification(PushPlusConfig{Token: "token"}, "title", "content"); err != nil {
		t.Fatal(err)
	}
}

func TestPushPlusConfigExtraction(t *testing.T) {
	config, err := parseNotificationConfigs(`{"enabled":true,"type":"pushplus","repeat_enabled":true,"repeat_interval":99,"pushplus":{"token":" token ","channel":" cp ","option":" route ","to":" userID ","webhook_url":"https://ignored.invalid"},"email":{"to":"wrong"},"feishu":{"webhook_url":"https://ignored.invalid"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || !config.RepeatEnabled || config.RepeatInterval != 99 || config.Type != "pushplus" {
		t.Fatalf("top level config lost: %#v", config)
	}
	if config.PushPlus != (PushPlusConfig{Token: "token", Channel: "cp", Option: "route", To: "userID"}) || config.WebhookURL != "" || config.Secret != "" || config.Email.To != "" {
		t.Fatalf("wrong type config extracted: %#v", config)
	}
	for _, raw := range []string{
		`{"type":"pushplus","pushplus":{"token":123}}`,
		`{"type":"pushplus","pushplus":{"channel":["secret-token"]}}`,
		`{"type":"pushplus","pushplus":"secret-token"}`,
	} {
		if _, err := parseNotificationConfigs(raw); err == nil || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("bad typed config not safely rejected: %v", err)
		}
	}
	for _, raw := range []string{`{"type":"pushplus"}`, `{"enabled":false,"type":"pushplus","pushplus":{"token":" "}}`} {
		config, err := parseNotificationConfigs(raw)
		if err != nil || config.PushPlus.Channel != "wechat" || config.PushPlus.Token != "" {
			t.Fatalf("disabled/incomplete config should load: %#v %v", config, err)
		}
	}
	// Existing providers still extract their own nested fields unchanged.
	config, err = parseNotificationConfigs(`{"type":"email","email":{"smtp_host":"smtp.invalid","smtp_port":465,"to":"user@example.invalid"}}`)
	if err != nil || config.Email.SMTPHost != "smtp.invalid" || config.Email.SMTPPort != 465 || config.Email.To != "user@example.invalid" {
		t.Fatalf("email config changed: %#v %v", config, err)
	}
	config, err = parseNotificationConfigs(`{"type":"feishu","feishu":{"webhook_url":"https://example.invalid","secret":"test"}}`)
	if err != nil || config.WebhookURL != "https://example.invalid" || config.Secret != "test" {
		t.Fatalf("webhook config changed: %#v %v", config, err)
	}
}

func TestPushPlusCentralizedNotificationPaths(t *testing.T) {
	// An isolated in-memory test database; never opens the app's persisted database.
	store, err := storage.NewStorage(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.SetConfig("device_name", "测试主机"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfig("notification_configs", `{"enabled":true,"type":"pushplus","repeat_enabled":true,"repeat_interval":10,"pushplus":{"token":"token"}}`); err != nil {
		t.Fatal(err)
	}
	type message struct{ Title, Content string }
	var messages []message
	n := NewNotifier(store)
	n.pushPlusClient = &http.Client{Transport: pushPlusRoundTripper(func(r *http.Request) (*http.Response, error) {
		var m message
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
		return pushPlusResponse(200, `{"code":200}`), nil
	})}
	n.NotifyRemoteStart(nil)
	n.NotifyRemoteEnd(nil)
	n.NotifyRemoteOngoing(nil, "1 分钟")
	n.NotifyAppExit()
	n.NotifySecurityEvent("安全事件", []string{"详情：测试"})
	if err := n.SendResetCode("123456", 5); err != nil {
		t.Fatal(err)
	}
	if err := n.SendTestNotification(NotificationConfig{Type: "pushplus", PushPlus: PushPlusConfig{Token: "token"}}); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 7 {
		t.Fatalf("some paths did not dispatch to PushPlus: %d", len(messages))
	}
	if !strings.Contains(messages[5].Title, "密码重置验证码") || !strings.Contains(messages[5].Content, "123456") || !strings.Contains(messages[5].Content, "5 分钟") {
		t.Fatal("reset code content did not reach the centralized sender")
	}
	if n.RepeatInterval() != 10*time.Second {
		t.Fatal("repeat settings changed")
	}
}
