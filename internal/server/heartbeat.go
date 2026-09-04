package server

import (
	"log"
	"time"
)

// 界面心跳看门狗。
//
// Electron 每 2 秒来问一次 /api/status，这就是天然的心跳。密码闸门挡得住
// "顺手点退出"，挡不住"任务管理器直接结束进程"——守护进程是独立进程，
// 界面被杀之后它还活着，于是由它来发现并告警。
const (
	uiHeartbeatTimeout  = 30 * time.Second // 超过这么久没心跳就判定界面已被终止
	uiHeartbeatInterval = 10 * time.Second // 看门狗检查间隔
)

// touchHeartbeat 在每次 /api/status 被调用时刷新界面心跳。
func (s *Server) touchHeartbeat() {
	s.seenMu.Lock()
	wasAlerted := s.uiAlerted
	s.uiAlerted = false
	s.lastSeen = time.Now()
	s.seenMu.Unlock()

	// 界面回来了：补一条恢复通知，免得机主一直悬着
	if wasAlerted {
		log.Printf("[心跳] 界面进程已恢复")
		if s.security != nil {
			s.security.Alert("界面进程已恢复",
				"事件：界面已重新连上守护进程",
				"监控：全程未中断")
		}
	}
}

// watchUIHeartbeat 常驻检查界面心跳，由 Start() 拉起。
func (s *Server) watchUIHeartbeat() {
	ticker := time.NewTicker(uiHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.checkUIHeartbeat()
		}
	}
}

// checkUIHeartbeat 判断界面是否已经失联；只在启用了密码保护时告警，
// 否则用户正常关掉界面也会收到消息，属于噪音。
func (s *Server) checkUIHeartbeat() {
	if s.security == nil || !s.security.Enabled() {
		return
	}

	s.seenMu.Lock()
	last := s.lastSeen
	if last.IsZero() || s.uiAlerted || time.Since(last) <= uiHeartbeatTimeout {
		s.seenMu.Unlock()
		return
	}
	s.uiAlerted = true
	s.seenMu.Unlock()

	log.Printf("[心跳] 界面已失联 %.0f 秒，判定为被强行结束", time.Since(last).Seconds())
	s.security.Alert("界面进程已被终止",
		"事件：界面已停止心跳，可能被任务管理器强行结束",
		"守护进程：仍在运行，检测与通知未中断",
		"提示：若不是你本人操作，请立即检查这台机器")
}
