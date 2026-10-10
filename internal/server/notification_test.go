package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"RemoteKnown/internal/notifier"
	"RemoteKnown/internal/storage"
)

func newNotificationServer(t *testing.T) *Server {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "notification.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return NewServer(nil, store, notifier.NewNotifier(store), nil)
}

func notificationRequest(t *testing.T, handler http.HandlerFunc, method string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var text string
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		text = string(raw)
	}
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(method, "/api/notification", strings.NewReader(text)))
	return rec
}

func TestNotificationPushPlusDefaultsAndLegacy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		config string
	}{
		{"empty", "", ""},
		{"existing channels", "notification_configs", `{"enabled":true,"type":"dingtalk","dingtalk":{"webhook_url":"https://example.invalid/bot","secret":"test-secret"}}`},
		{"legacy migration", "notification_config", `{"enabled":true,"type":"feishu","webhook_url":"https://example.invalid/bot","secret":"test-secret"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNotificationServer(t)
			if tc.key != "" {
				if err := srv.storage.SetConfig(tc.key, tc.config); err != nil {
					t.Fatal(err)
				}
			}
			rec := notificationRequest(t, srv.handleNotification, http.MethodGet, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			var result struct {
				PushPlus       notifier.PushPlusConfig `json:"pushplus"`
				RepeatInterval int                     `json:"repeat_interval"`
				Type           string                  `json:"type"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.PushPlus.Channel != "wechat" || result.PushPlus.Token != "" || result.RepeatInterval != notifier.DefaultRepeatInterval {
				t.Fatalf("unexpected defaults: %+v", result)
			}
			if tc.name == "existing channels" && result.Type != "dingtalk" {
				t.Fatal("existing active channel changed")
			}
		})
	}
}

func TestNotificationPushPlusSavePreservesOtherChannels(t *testing.T) {
	srv := newNotificationServer(t)
	original := map[string]interface{}{
		"enabled": true, "type": "email",
		"feishu":   map[string]interface{}{"webhook_url": "https://example.invalid/feishu", "secret": "test-feishu"},
		"dingtalk": map[string]interface{}{"webhook_url": "https://example.invalid/dingtalk", "secret": "test-dingtalk"},
		"email":    map[string]interface{}{"smtp_host": "mail.example.invalid", "password": "test-password"},
	}
	raw, _ := json.Marshal(original)
	if err := srv.storage.SetConfig("notification_configs", string(raw)); err != nil {
		t.Fatal(err)
	}
	body := map[string]interface{}{
		"enabled": true, "type": "pushplus",
		"pushplus":       map[string]interface{}{"token": " test-token ", "channel": " cp ", "option": " audit-app ", "to": " operator "},
		"repeat_enabled": true, "repeat_interval": 1,
	}
	rec := notificationRequest(t, srv.handleNotification, http.MethodPost, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = notificationRequest(t, srv.handleNotification, http.MethodGet, nil)
	var result map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"feishu", "dingtalk", "email"} {
		if !reflect.DeepEqual(result[key], original[key]) {
			t.Fatalf("%s changed: %v", key, result[key])
		}
	}
	if result["type"] != "pushplus" || result["repeat_enabled"] != true || result["repeat_interval"] != float64(notifier.MinRepeatInterval) {
		t.Fatalf("global config changed incorrectly: %v", result)
	}
	want := map[string]interface{}{"token": "test-token", "channel": "cp", "option": "audit-app", "to": "operator"}
	if !reflect.DeepEqual(result["pushplus"], want) {
		t.Fatalf("saved PushPlus = %v", result["pushplus"])
	}
	body = map[string]interface{}{"enabled": true, "type": "feishu", "webhook_url": "https://example.invalid/new-bot"}
	rec = notificationRequest(t, srv.handleNotification, http.MethodPost, body)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	rec = notificationRequest(t, srv.handleNotification, http.MethodGet, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result["pushplus"], want) {
		t.Fatal("switching to another channel erased PushPlus config")
	}
}

func TestNotificationPushPlusValidationDoesNotSave(t *testing.T) {
	srv := newNotificationServer(t)
	original := `{"enabled":false,"type":"feishu","feishu":{"webhook_url":"https://example.invalid/bot"}}`
	if err := srv.storage.SetConfig("notification_configs", original); err != nil {
		t.Fatal(err)
	}
	for _, config := range []notifier.PushPlusConfig{
		{}, {Token: "  "}, {Token: "test-token", Channel: "unknown"},
		{Token: "test-token", Channel: "cp"}, {Token: "test-token", Channel: "webhook"},
	} {
		for _, test := range []bool{false, true} {
			handler := srv.handleNotification
			if test {
				handler = srv.handleTestNotification
			}
			rec := notificationRequest(t, handler, http.MethodPost, map[string]interface{}{"enabled": true, "type": "pushplus", "pushplus": config})
			if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("invalid config status = %d: %s", rec.Code, rec.Body.String())
			}
			var result struct {
				Success bool   `json:"success"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success || result.Error == "" || strings.Contains(result.Error, "test-token") {
				t.Fatal("invalid or sensitive error response")
			}
		}
	}
	stored, err := srv.storage.GetConfig("notification_configs")
	if err != nil || stored != original {
		t.Fatalf("invalid request modified stored config: %v", err)
	}
	rec := notificationRequest(t, srv.handleNotification, http.MethodPost, map[string]interface{}{"enabled": false, "type": "pushplus"})
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled empty config should be allowed: %s", rec.Body.String())
	}
}

type notificationTransportFunc func(*http.Request) (*http.Response, error)

func (f notificationTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNotificationPushPlusTestDispatch(t *testing.T) {
	srv := newNotificationServer(t)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	var captured map[string]interface{}
	response := `{"code":200,"msg":"请求成功","data":"test-message-id"}`
	http.DefaultTransport = notificationTransportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://www.pushplus.plus/send" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected outbound request: %s %s", r.Method, r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})
	for _, channel := range []string{"", "wechat", "app", "extension", "webhook", "clawbot", "cmcc", "qq", "cp", "mail", "sms", "voice"} {
		config := notifier.PushPlusConfig{Token: "test-token", Channel: channel}
		if channel == "webhook" || channel == "cp" {
			config.Option = "audit-channel"
		}
		if channel == "qq" {
			// 编码也可能对应自定义机器人，不应假定它一定是QQ群。
			config.Option = "configured-bot"
			config.To = "friend-token"
		}
		rec := notificationRequest(t, srv.handleTestNotification, http.MethodPost, map[string]interface{}{"type": "pushplus", "pushplus": config})
		if rec.Code != http.StatusOK {
			t.Fatalf("channel %q test failed: %s", channel, rec.Body.String())
		}
		var result struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Success || !strings.Contains(result.Message, "接收推送请求") {
			t.Fatalf("async acceptance message = %+v", result)
		}
		if captured["channel"] != config.Normalized().Channel || captured["token"] != "test-token" || captured["template"] != "txt" {
			t.Fatalf("outbound payload fields = %v", captured)
		}
		if channel == "qq" && (captured["option"] != "configured-bot" || captured["to"] != "friend-token") {
			t.Fatal("QQ channel target fields were discarded")
		}
		if !strings.Contains(captured["content"].(string), "这是一条测试通知") {
			t.Fatal("test content missing")
		}
		captured = nil
	}
	response = `{"code":600,"msg":"bad test-token","data":null}`
	rec := notificationRequest(t, srv.handleTestNotification, http.MethodPost, map[string]interface{}{"type": "pushplus", "pushplus": notifier.PushPlusConfig{Token: "test-token"}})
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "test-token") {
		t.Fatalf("sensitive failure or wrong status: %s", rec.Body.String())
	}
}
