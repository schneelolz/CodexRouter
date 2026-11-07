package router

import (
	"net/http"
	"time"

	"codexrouter/internal/api"
)

// renewSession 在锁内再次核对认证，避免并发引导换会话后旧请求刷新旧Cookie。
func (s *Server) renewSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("codexrouter_session")
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if err != nil || !now.Before(s.sessionExpiry) || !equal(s.session, digest(cookie.Value)) {
		reason := "cookie_mismatch"
		if err != nil {
			reason = "cookie_missing"
		} else if !now.Before(s.sessionExpiry) {
			reason = "session_expired"
		}
		s.authEvent("auth_rejected", reason)
		fail(w, 401, "AUTH_REQUIRED", "管理认证已失效，请从托盘重新打开管理页面")
		return
	}
	if !s.origins[r.Header.Get("Origin")] || !equal(s.csrf, r.Header.Get("X-CSRF-Token")) {
		reason := "csrf_mismatch"
		if !s.origins[r.Header.Get("Origin")] {
			reason = "origin_mismatch"
		}
		s.authEvent("auth_rejected", reason)
		fail(w, 403, "CSRF_REQUIRED", "认证续期需要同源Origin与CSRF凭据")
		return
	}
	if s.sessionExpiry.Sub(now) < time.Hour {
		s.sessionExpiry = now.Add(8 * time.Hour)
		s.authEvent("session_renewed", "extended")
		// 不轮换令牌，其他已打开的管理页继续使用原会话与CSRF。
		http.SetCookie(w, &http.Cookie{Name: cookie.Name, Value: cookie.Value, Path: "/api/v1/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
	}
	s.authEvent("session_renew_checked", "ok")
	respond(w, 200, api.AuthSession{CSRFToken: s.csrf})
}
