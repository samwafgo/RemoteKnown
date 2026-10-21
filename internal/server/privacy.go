package server

import (
	"errors"
	"net/http"

	"RemoteKnown/internal/privacyscreen"
)

// SetPrivacy 装配隐私屏管理器。用 setter 而不是加到 NewServer 参数里，
// 免得动到已有调用方的签名。
func (s *Server) SetPrivacy(p *privacyscreen.Manager) {
	s.privacy = p
}

// privacyReady 隐私屏未装配时统一回一个明确的错误，而不是 panic
func (s *Server) privacyReady(w http.ResponseWriter) bool {
	if s.privacy == nil {
		writeSecErr(w, http.StatusServiceUnavailable, "隐私屏模块未启用")
		return false
	}
	return true
}

// handlePrivacyStatus 当前运行状态：是否支持、是否生效、是否在等解锁
func (s *Server) handlePrivacyStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.privacyReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.privacy.Status())
}

// handlePrivacyConfig 读写隐私屏配置
func (s *Server) handlePrivacyConfig(w http.ResponseWriter, r *http.Request) {
	if !s.privacyReady(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"options": s.privacy.Options(),
			"presets": privacyscreen.BuiltinPresets,
		})

	case http.MethodPost:
		// 改设置属于敏感操作，开了密码保护就要求已验证身份，
		// 和「安全」页其余设置项保持一致
		if s.security != nil && s.security.Enabled() &&
			!s.security.ValidateToken(r.Header.Get(unlockTokenHeader)) {
			writeSecErr(w, http.StatusUnauthorized, "请先验证密码")
			return
		}
		var opt privacyscreen.Options
		if !decodeBody(w, r, &opt) {
			return
		}
		if err := s.privacy.SaveOptions(opt); err != nil {
			writeSecErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"config":  s.privacy.Options(),
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePrivacyEnable 手动开启遮罩。
// 内部会先自检排除标志再显示窗口，自检不过直接返回失败原因并已发出告警。
func (s *Server) handlePrivacyEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.privacyReady(w) {
		return
	}
	if s.privacy.Options().Trigger == privacyscreen.TriggerOff {
		writeSecErr(w, http.StatusBadRequest, "隐私屏触发方式为「关闭」，请先在设置里启用")
		return
	}
	if err := s.privacy.Enable("手动开启"); err != nil {
		writeSecErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"status":  s.privacy.Status(),
	})
}

// handlePrivacyAuto 确认提示窗口已显示，或仅取消本次自动遮罩。
func (s *Server) handlePrivacyAuto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.privacyReady(w) {
		return
	}
	var req struct {
		RequestID string `json:"request_id"`
		Action    string `json:"action"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.RequestID == "" || (req.Action != "shown" && req.Action != "cancel") {
		writeSecErr(w, http.StatusBadRequest, "请提供 request_id 和有效的 action（shown/cancel）")
		return
	}
	if err := s.privacy.AutoAction(req.RequestID, req.Action); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, privacyscreen.ErrAutoRequestExpired) {
			code = http.StatusConflict
		}
		writeSecErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"status":  s.privacy.Status(),
	})
}

// handlePrivacyDisable 撤除遮罩。
// 配了「退出需要密码」且密码保护已启用时，这里必须先过密码校验。
func (s *Server) handlePrivacyDisable(w http.ResponseWriter, r *http.Request) {
	if !s.privacyReady(w) {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	needPw := s.privacy.Options().RequirePassword &&
		s.security != nil && s.security.Enabled()
	if needPw {
		// 两条身份来路都认：解锁小窗验过密码后给的令牌，或直接带上密码。
		// 有令牌就不再走一次 Verify，免得把失败次数重复计上去。
		if !s.security.ValidateToken(r.Header.Get(unlockTokenHeader)) {
			if _, err := s.security.Verify(req.Password, "privacy_disable"); err != nil {
				writeVerifyErr(w, err)
				return
			}
		}
	}

	s.privacy.Disable("手动撤除")
	s.privacy.ClearUnlockPending()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"status":  s.privacy.Status(),
	})
}

// handlePrivacyUnlockAck 前端已经把解锁框弹出来了，清掉热键标记，
// 免得下一次轮询又重复弹一遍
func (s *Server) handlePrivacyUnlockAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.privacyReady(w) {
		return
	}
	s.privacy.ClearUnlockPending()
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}
