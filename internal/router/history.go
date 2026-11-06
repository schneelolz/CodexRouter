package router

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"codexrouter/internal/api"
	"codexrouter/internal/history"
)

// initializeHistory 先校准绑定索引，再恢复尚未登记的最少元数据。
func (s *state) initializeHistory() error {
	if err := s.syncHistory(); err != nil {
		return err
	}
	cursor := ""
	for {
		page, err := s.history.Sessions("pending", 100, cursor)
		if err != nil {
			return err
		}
		for _, p := range page.Pending {
			if !validID(p.ID) || len(s.pending) >= 256 {
				return errors.New("待登记历史不兼容或超限")
			}
			s.pending[p.ID] = p
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

// syncHistory 只同步投影；持锁避免绑定并发产生旧快照覆盖。
func (s *state) syncHistory() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bindings := make([]api.Binding, 0, len(s.disk.Bindings))
	for _, b := range s.disk.Bindings {
		bindings = append(bindings, b)
	}
	if err := s.history.SyncBindings(bindings); err != nil {
		s.historyDirty = true
		return err
	}
	s.historyDirty = false
	return nil
}

func (s *Server) historyFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, history.ErrInvalidCursor) {
		fail(w, 400, "INVALID_PAGINATION", "分页参数或游标无效，请刷新后重试")
		return
	}
	s.historyDegraded.Store(true)
	fail(w, 503, "HISTORY_UNAVAILABLE", "历史存储暂不可用；记录可能不完整，固定路由绑定未被清除")
}

func (s *Server) manageHistory(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	if (r.URL.Path == "/api/v1/requests" || r.URL.Path == "/api/v1/sessions/page") && r.Method == http.MethodGet {
		q, err := parseHistoryQuery(r)
		if err != nil {
			s.historyFailure(w, history.ErrInvalidCursor)
			return
		}
		if r.URL.Path == "/api/v1/requests" {
			filter := r.URL.Query().Get("filter")
			if filter == "" {
				filter = "all"
			}
			var page api.RequestPage
			if q.page > 0 {
				page, err = s.history.RequestNumberedPage(q.limit, q.page, q.snapshot, filter)
			} else {
				page, err = s.history.Requests(q.limit, q.cursor, filter)
			}
			if err != nil {
				s.historyFailure(w, err)
				return
			}
			page.Degraded = s.historyDegraded.Load()
			respond(w, 200, page)
			return
		}
		kind := r.URL.Query().Get("kind")
		if kind == "" {
			kind = "bound"
		}
		s.state.mu.Lock()
		dirty := s.state.historyDirty
		s.state.mu.Unlock()
		if dirty {
			if err = s.state.syncHistory(); err != nil {
				s.historyFailure(w, err)
				return
			}
		}
		page, err := s.history.Sessions(kind, q.limit, q.cursor)
		if err != nil {
			s.historyFailure(w, err)
			return
		}
		respond(w, 200, page)
		return
	}
	if r.URL.Path == "/api/v1/history/policy" {
		if r.Method == http.MethodGet {
			days, err := s.history.Policy()
			if err != nil {
				s.historyFailure(w, err)
				return
			}
			respond(w, 200, api.HistoryPolicy{Days: days})
			return
		}
		if r.Method == http.MethodPut {
			var input struct {
				Days *int `json:"days"`
			}
			if !decode(w, r, &input) {
				return
			}
			if input.Days == nil || *input.Days < 0 || *input.Days > 3650 {
				fail(w, 400, "INVALID_RETENTION", "保留天数须为0至3650，0表示不限期")
				return
			}
			if _, err := s.history.SetPolicy(*input.Days, now); err != nil {
				s.historyFailure(w, err)
				return
			}
			respond(w, 200, api.HistoryPolicy{Days: *input.Days})
			return
		}
	}
	if r.URL.Path == "/api/v1/history/clear" && r.Method == http.MethodPost {
		var input api.ClearHistoryInput
		if !decode(w, r, &input) {
			return
		}
		if !input.Confirm {
			fail(w, 400, "CONFIRM_REQUIRED", "请确认只清除已结束的请求历史，活动请求和会话绑定将保留")
			return
		}
		deleted, err := s.history.Clear(now)
		if err != nil {
			s.historyFailure(w, err)
			return
		}
		respond(w, 200, api.ClearHistoryResult{Deleted: deleted})
		return
	}
	fail(w, 405, "METHOD_NOT_ALLOWED", "该历史接口不支持此操作")
}

type historyQuery struct {
	limit    int
	cursor   string
	page     int
	snapshot string
}

func parseHistoryQuery(r *http.Request) (historyQuery, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return historyQuery{}, history.ErrInvalidCursor
	}
	out := historyQuery{limit: 20, cursor: q.Get("cursor")}
	for key, values := range q {
		if len(values) != 1 || (key != "limit" && key != "cursor" && key != "filter" && key != "kind" && key != "page" && key != "snapshot") {
			return out, history.ErrInvalidCursor
		}
	}
	if q.Has("page") {
		n, err := strconv.Atoi(q.Get("page"))
		if err != nil || n < 1 || n > 1000000 || q.Has("cursor") || r.URL.Path != "/api/v1/requests" {
			return out, history.ErrInvalidCursor
		}
		out.page = n
	}
	if q.Has("snapshot") {
		if out.page == 0 || len(q.Get("snapshot")) > 1024 {
			return out, history.ErrInvalidCursor
		}
		out.snapshot = q.Get("snapshot")
	}
	if value, exists := q["limit"]; exists {
		n, err := strconv.Atoi(value[0])
		if err != nil || (n != 20 && n != 50 && n != 100) {
			return out, history.ErrInvalidCursor
		}
		out.limit = n
	}
	if (r.URL.Path == "/api/v1/requests" && q.Has("kind")) || (r.URL.Path == "/api/v1/sessions/page" && q.Has("filter")) {
		return out, history.ErrInvalidCursor
	}
	if len(out.cursor) > 1024 {
		return out, history.ErrInvalidCursor
	}
	return out, nil
}
