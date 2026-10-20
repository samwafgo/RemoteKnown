package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"RemoteKnown/internal/privacyscreen"
	"RemoteKnown/internal/security"
	"RemoteKnown/internal/storage"
)

func newPrivacyServer(t *testing.T, withPassword bool) (*Server, *fakeAlerter) {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化临时存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	alerter := &fakeAlerter{}
	sec := security.NewManager(store, alerter)
	if withPassword {
		if _, err := sec.Setup("s3cret"); err != nil {
			t.Fatalf("Setup 失败: %v", err)
		}
		alerter.titlesAfter(1)
		alerter.mu.Lock()
		alerter.titles = nil
		alerter.mu.Unlock()
	}

	srv := NewServer(nil, store, nil, sec)
	srv.SetPrivacy(privacyscreen.NewManager(store, alerter))
	t.Cleanup(srv.privacy.Shutdown)
	return srv, alerter
}

func doJSON(t *testing.T, h http.HandlerFunc, method, body string, hdr map[string]string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(method, "/", bytes.NewBufferString(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)

	var out map[string]interface{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// 出厂默认必须是「关闭」：这个功能会遮住整块屏幕，不该有任何默认介入。
func TestPrivacyDefaultTriggerIsOff(t *testing.T) {
	srv, _ := newPrivacyServer(t, false)

	code, body := doJSON(t, srv.handlePrivacyConfig, http.MethodGet, "", nil)
	if code != http.StatusOK {
		t.Fatalf("读配置状态码 = %d, 期望 200", code)
	}
	if body["trigger"] != privacyscreen.TriggerOff {
		t.Errorf("默认触发方式 = %v, 期望 %q", body["trigger"], privacyscreen.TriggerOff)
	}
	if body["auto_countdown"] != false {
		t.Errorf("倒计时默认应为关闭，实际 %v", body["auto_countdown"])
	}
	if body["lock_on_end"] != false {
		t.Errorf("默认 lock_on_end = %v, 期望 false（必须用户主动勾选）", body["lock_on_end"])
	}
}

// 触发方式为「关闭」时，开启接口应当直接拒绝，而不是把屏幕遮上
func TestPrivacyEnableRejectedWhenTriggerOff(t *testing.T) {
	srv, _ := newPrivacyServer(t, false)

	code, body := doJSON(t, srv.handlePrivacyEnable, http.MethodPost, "{}", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", code)
	}
	if body["success"] != false {
		t.Errorf("success = %v, 期望 false", body["success"])
	}
}

// 配置往返：写进去什么，读出来就该是什么
func TestPrivacyConfigRoundTrip(t *testing.T) {
	srv, _ := newPrivacyServer(t, false)

	in := `{"trigger":"manual","auto_countdown":true,"bg_color":"#0B3D91","fg_color":"#FFFFFF",
	        "font_size":56,"bold":true,"hotkey":"Ctrl+Alt+P",
	        "max_minutes":15,"lock_on_end":true,"fit":"contain"}`
	code, _ := doJSON(t, srv.handlePrivacyConfig, http.MethodPost, in, nil)
	if code != http.StatusOK {
		t.Fatalf("写配置状态码 = %d, 期望 200", code)
	}

	code, body := doJSON(t, srv.handlePrivacyConfig, http.MethodGet, "", nil)
	if code != http.StatusOK {
		t.Fatalf("读配置状态码 = %d, 期望 200", code)
	}
	checks := map[string]interface{}{
		"trigger":        "manual",
		"auto_countdown": true,
		"bg_color":       "#0B3D91",
		"font_size":      float64(56),
		"max_minutes":    float64(15),
		"lock_on_end":    true,
		"fit":            "contain",
	}
	for k, want := range checks {
		if body[k] != want {
			t.Errorf("%s = %v, 期望 %v", k, body[k], want)
		}
	}
}

// 非法取值要被纠正，而不是原样落库害得引擎起不来
func TestPrivacyConfigSanitizesBadValues(t *testing.T) {
	srv, _ := newPrivacyServer(t, false)

	in := `{"trigger":"whatever","bg_color":"not-a-color","fit":"diagonal","font_size":9999}`
	if code, _ := doJSON(t, srv.handlePrivacyConfig, http.MethodPost, in, nil); code != http.StatusOK {
		t.Fatalf("写配置状态码 = %d, 期望 200", code)
	}

	_, body := doJSON(t, srv.handlePrivacyConfig, http.MethodGet, "", nil)
	if body["trigger"] != privacyscreen.TriggerOff {
		t.Errorf("非法 trigger 应回落为 off, 实际 %v", body["trigger"])
	}
	if body["fit"] != "cover" {
		t.Errorf("非法 fit 应回落为 cover, 实际 %v", body["fit"])
	}
	if body["bg_color"] == "not-a-color" {
		t.Errorf("非法颜色未被纠正: %v", body["bg_color"])
	}
	if body["font_size"] == float64(9999) {
		t.Errorf("越界字号未被纠正: %v", body["font_size"])
	}
}

// 开了密码保护后，改隐私屏设置必须先验证身份
func TestPrivacyConfigRequiresUnlockToken(t *testing.T) {
	srv, _ := newPrivacyServer(t, true)

	code, _ := doJSON(t, srv.handlePrivacyConfig, http.MethodPost, `{"trigger":"manual"}`, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("无令牌改配置状态码 = %d, 期望 401", code)
	}

	token, err := srv.security.Verify("s3cret", "options")
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	code, _ = doJSON(t, srv.handlePrivacyConfig, http.MethodPost, `{"trigger":"manual"}`,
		map[string]string{unlockTokenHeader: token})
	if code != http.StatusOK {
		t.Fatalf("带令牌改配置状态码 = %d, 期望 200", code)
	}
}

// 撤除遮罩：密码错要挡住，密码对或带有效令牌都要放行
func TestPrivacyDisableChecksPassword(t *testing.T) {
	srv, _ := newPrivacyServer(t, true)

	token, err := srv.security.Verify("s3cret", "options")
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	// require_password 默认为 true，这里把触发方式打开即可
	if code, _ := doJSON(t, srv.handlePrivacyConfig, http.MethodPost,
		`{"trigger":"manual","require_password":true}`,
		map[string]string{unlockTokenHeader: token}); code != http.StatusOK {
		t.Fatalf("准备配置失败")
	}

	code, _ := doJSON(t, srv.handlePrivacyDisable, http.MethodPost, `{"password":"wrong"}`, nil)
	if code != http.StatusUnauthorized {
		t.Errorf("错误密码状态码 = %d, 期望 401", code)
	}

	code, _ = doJSON(t, srv.handlePrivacyDisable, http.MethodPost, `{"password":"s3cret"}`, nil)
	if code != http.StatusOK {
		t.Errorf("正确密码状态码 = %d, 期望 200", code)
	}

	code, _ = doJSON(t, srv.handlePrivacyDisable, http.MethodPost, `{}`,
		map[string]string{unlockTokenHeader: token})
	if code != http.StatusOK {
		t.Errorf("带令牌撤除状态码 = %d, 期望 200", code)
	}
}

func TestPrivacyAutoShownAndCancelWithoutPassword(t *testing.T) {
	srv, _ := newPrivacyServer(t, true)
	o := privacyscreen.DefaultOptions()
	o.Trigger = privacyscreen.TriggerAuto
	o.AutoCountdown = true
	if err := srv.privacy.SaveOptions(o); err != nil {
		t.Fatal(err)
	}
	srv.privacy.OnRemoteStart()
	st := srv.privacy.Status()
	if !st.AutoPending || st.AutoRequestID == "" {
		t.Fatalf("缺少待处理请求: %+v", st)
	}
	if _, err := time.Parse(time.RFC3339Nano, st.AutoDeadline); err != nil {
		t.Fatalf("deadline 格式错误: %v", err)
	}
	request := func(id, action string) (int, map[string]interface{}) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"request_id": id, "action": action})
		return doJSON(t, srv.handlePrivacyAuto, http.MethodPost, string(body), nil)
	}
	if code, _ := request("old", "cancel"); code != http.StatusConflict {
		t.Fatalf("旧请求状态码 = %d", code)
	}
	code, body := request(st.AutoRequestID, "shown")
	if code != http.StatusOK || body["success"] != true {
		t.Fatalf("shown 失败: %d %v", code, body)
	}
	status, ok := body["status"].(map[string]interface{})
	if !ok || status["auto_pending"] != true || status["auto_request_id"] != st.AutoRequestID {
		t.Fatalf("shown 未返回当前状态: %v", body)
	}
	deadline := status["auto_deadline"]
	code, body = request(st.AutoRequestID, "shown")
	if code != http.StatusOK || body["status"].(map[string]interface{})["auto_deadline"] != deadline {
		t.Fatalf("重复 shown 改变倒计时: %d %v", code, body)
	}
	code, body = request(st.AutoRequestID, "cancel")
	if code != http.StatusOK || body["success"] != true || body["status"].(map[string]interface{})["auto_pending"] != false {
		t.Fatalf("无密码 cancel 失败: %d %v", code, body)
	}
	if code, _ := request(st.AutoRequestID, "cancel"); code != http.StatusConflict {
		t.Fatalf("已取消请求状态码 = %d", code)
	}
	srv.privacy.OnRemoteStart()
	if srv.privacy.Status().AutoPending {
		t.Fatal("同会话取消后再次提示")
	}
	srv.privacy.OnRemoteEnd()
	srv.privacy.OnRemoteStart()
	if code, _ := request(st.AutoRequestID, "shown"); code != http.StatusConflict {
		t.Fatalf("上一会话请求状态码 = %d", code)
	}
}

func TestPrivacyAutoValidation(t *testing.T) {
	srv, _ := newPrivacyServer(t, false)
	if code, _ := doJSON(t, srv.handlePrivacyAuto, http.MethodGet, "", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 状态码 = %d", code)
	}
	for _, body := range []string{`{}`, `{"request_id":"id","action":"enable"}`, `{"action":"cancel"}`, `{`} {
		if code, _ := doJSON(t, srv.handlePrivacyAuto, http.MethodPost, body, nil); code != http.StatusBadRequest {
			t.Errorf("无效请求 %s 状态码 = %d", body, code)
		}
	}
	if code, _ := doJSON(t, (&Server{}).handlePrivacyAuto, http.MethodPost, `{}`, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("未装配模块状态码 = %d", code)
	}
}

// 状态接口要如实报告系统是否支持，不支持时必须给出原因
func TestPrivacyStatusReportsSupport(t *testing.T) {
	srv, _ := newPrivacyServer(t, false)

	code, body := doJSON(t, srv.handlePrivacyStatus, http.MethodGet, "", nil)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", code)
	}
	if _, ok := body["supported"]; !ok {
		t.Fatal("响应里缺少 supported 字段")
	}
	if body["supported"] == false && body["reason"] == "" {
		t.Error("不支持时必须给出 reason，否则用户不知道为什么用不了")
	}
	if body["active"] != false {
		t.Errorf("初始 active = %v, 期望 false", body["active"])
	}
}
