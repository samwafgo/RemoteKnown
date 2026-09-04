package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"RemoteKnown/internal/security"
)

// unlockTokenHeader 是携带解锁令牌的请求头，用于保护"改设置"这类需要已验证身份的接口。
const unlockTokenHeader = "X-Unlock-Token"

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeSecErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{"success": false, "error": msg})
}

// decodeBody 解析 POST JSON 请求体，失败时直接写回 400。
func decodeBody(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		log.Printf("[安全API] 解析请求失败: %v", err)
		writeSecErr(w, http.StatusBadRequest, "请求格式不正确")
		return false
	}
	return true
}

// writeVerifyErr 把 security 包的错误翻译成前端能直接展示的响应。
// 密码错误用 401、锁定用 429，其余按项目惯例走 500。
func writeVerifyErr(w http.ResponseWriter, err error) {
	var locked *security.ErrLocked
	var wrong *security.ErrWrongPassword
	switch {
	case errors.As(err, &locked):
		writeJSON(w, http.StatusTooManyRequests, map[string]interface{}{
			"success":      false,
			"error":        locked.Error(),
			"locked":       true,
			"locked_until": locked.Until.Format("15:04"),
		})
	case errors.As(err, &wrong):
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success":       false,
			"error":         wrong.Error(),
			"attempts_left": wrong.AttemptsLeft,
		})
	default:
		writeSecErr(w, http.StatusInternalServerError, err.Error())
	}
}

// handleSecurityState 返回当前安全状态，前端据此决定是否加闸门、是否静默通知。
func (s *Server) handleSecurityState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.security.State())
}

// handleSecuritySetup 首次设置密码，返回一次性恢复码。
func (s *Server) handleSecuritySetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	recovery, err := s.security.Setup(req.Password)
	if err != nil {
		writeSecErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"message":       "密码保护已启用",
		"recovery_code": recovery,
	})
}

// handleSecurityVerify 校验密码，成功返回解锁令牌。
func (s *Server) handleSecurityVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Action   string `json:"action"` // 用于告警文案，如"退出程序"
	}
	if !decodeBody(w, r, &req) {
		return
	}
	token, err := s.security.Verify(req.Password, req.Action)
	if err != nil {
		writeVerifyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"token":   token,
	})
}

// handleSecurityChange 修改密码。
func (s *Server) handleSecurityChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.security.Change(req.OldPassword, req.NewPassword); err != nil {
		writeVerifyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "密码已修改"})
}

// handleSecurityDisable 关闭密码保护。
func (s *Server) handleSecurityDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.security.Disable(req.Password); err != nil {
		writeVerifyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "密码保护已关闭"})
}

// handleSecurityOptions 修改静默 / 托盘开关。保护生效时需要携带解锁令牌。
func (s *Server) handleSecurityOptions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SilentLocal *bool `json:"silent_local"`
		LockTray    *bool `json:"lock_tray"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if s.security.Enabled() && !s.security.ValidateToken(r.Header.Get(unlockTokenHeader)) {
		writeSecErr(w, http.StatusUnauthorized, "需要先验证密码")
		return
	}
	if err := s.security.SetOptions(req.SilentLocal, req.LockTray); err != nil {
		writeSecErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "已保存"})
}

// handleSecurityRecoveryRegenerate 重新生成恢复码，旧的立即作废。
func (s *Server) handleSecurityRecoveryRegenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	recovery, err := s.security.RegenerateRecovery(req.Password)
	if err != nil {
		writeVerifyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"recovery_code": recovery,
	})
}

// handleSecurityResetRequest 通过已配置的通知渠道发出重置验证码。
func (s *Server) handleSecurityResetRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.security.RequestReset(); err != nil {
		writeSecErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "验证码已发送到你配置的通知渠道",
	})
}

// handleSecurityResetConfirm 用验证码或恢复码重置密码。
func (s *Server) handleSecurityResetConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code         string `json:"code"`
		RecoveryCode string `json:"recovery_code"`
		NewPassword  string `json:"new_password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.security.ConfirmReset(req.Code, req.RecoveryCode, req.NewPassword); err != nil {
		writeSecErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "密码已重置",
		// 用恢复码重置后会换发新的恢复码，需要提醒机主重新保存。
		"recovery_code": s.security.TakeFreshRecovery(),
	})
}
