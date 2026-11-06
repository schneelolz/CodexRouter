package router

import (
	"codexrouter/internal/api"
	"codexrouter/internal/daily"
	"net/http"
)

func (s *Server) manageDaily(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/daily" && r.Method == http.MethodGet {
		if s.dailyManager == nil {
			respond(w, 200, daily.Status{State: "unavailable", Message: "当前实例未启用日常接入"})
			return
		}
		respond(w, 200, s.dailyManager.Status())
		return
	}
	if r.Method != http.MethodPost || r.URL.Path == "/api/v1/daily" {
		fail(w, 405, "METHOD_NOT_ALLOWED", "日常接入预览与应用仅支持POST")
		return
	}
	if s.dailyManager == nil {
		fail(w, 503, "DAILY_UNAVAILABLE", "当前实例未启用日常接入")
		return
	}
	if r.URL.Path == "/api/v1/daily/preview" {
		var in api.DailyAction
		if !decode(w, r, &in) {
			return
		}
		result, err := s.dailyManager.Preview(string(in.Action))
		if err != nil {
			fail(w, 409, err.Error(), daily.Message(err))
			return
		}
		respond(w, 200, result)
		return
	}
	var in api.DailyApply
	if !decode(w, r, &in) {
		return
	}
	if err := s.dailyManager.Apply(in.Token); err != nil {
		fail(w, 409, err.Error(), daily.Message(err))
		return
	}
	respond(w, 200, s.dailyManager.Status())
}
