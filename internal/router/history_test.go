package router

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"codexrouter/internal/api"
)

func TestHistoryManagementSecurityValidationAndBindingSurvivesClear(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	cookie, csrf := securityLogin(t, s)
	origin := "http://" + s.authority
	_, err := s.state.resolve("synthetic-history-thread")
	if err == nil {
		t.Fatal("未知历史不能自动绑定")
	}
	bound, err := s.state.register(api.RegisterInput{ID: "synthetic-history-thread", ProviderID: "official"})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := s.history.Begin("POST /responses", bound.ID)
	s.history.Finish(id, 200, "HTTP_200", 3)
	for _, path := range []string{"/api/v1/requests", "/api/v1/sessions/page", "/api/v1/history/policy"} {
		w := securityRequest(s, securityRequestOptions{path: path})
		if w.Code != 401 {
			t.Fatalf("无认证读取: %s %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/v1/requests?limit=1", "/api/v1/requests?limit=20&limit=50", "/api/v1/requests?cursor=bad", "/api/v1/requests?filter=invalid", "/api/v1/sessions/page?kind=all"} {
		w := securityRequest(s, securityRequestOptions{path: path, cookie: cookie})
		if w.Code != 400 {
			t.Fatalf("非法查询: %s %d", path, w.Code)
		}
	}
	for _, test := range []struct{ method, path, body string }{{"PUT", "/api/v1/history/policy", `{"days":1}`}, {"POST", "/api/v1/history/clear", `{"confirm":true}`}} {
		w := securityRequest(s, securityRequestOptions{method: test.method, path: test.path, body: test.body, cookie: cookie})
		if w.Code != 403 {
			t.Fatal("写入绕过CSRF")
		}
	}
	for _, test := range []struct{ path, body string }{{"policy", `{}`}, {"policy", `{"days":null}`}, {"policy", `{"days":1.5}`}, {"policy", `{"days":-1}`}, {"policy", `{"days":3651}`}, {"clear", `{"confirm":false}`}} {
		method := "PUT"
		if test.path == "clear" {
			method = "POST"
		}
		w := securityRequest(s, securityRequestOptions{method: method, path: "/api/v1/history/" + test.path, body: test.body, cookie: cookie, origin: origin, csrf: csrf})
		if w.Code != 400 {
			t.Fatal("非法清理输入被接受")
		}
	}
	w := securityRequest(s, securityRequestOptions{method: "POST", path: "/api/v1/history/clear", body: `{"confirm":true}`, cookie: cookie, origin: origin, csrf: csrf})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	provider, err := s.state.resolve(bound.ID)
	if err != nil || provider != "official" {
		t.Fatal("清理请求影响权威路由")
	}
	w = securityRequest(s, securityRequestOptions{path: "/api/v1/sessions/page?kind=bound", cookie: cookie})
	var page api.SessionPage
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 1 {
		t.Fatal("清理请求影响会话索引")
	}
}

func TestHistoryRestartPendingAndIndexFailureCannotChangeBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	s, p, creds := newSecurityServer(t, func(o *Options) { o.HistoryPath = path })
	_, _ = s.state.resolve("persist-pending")
	s.Close()
	next, err := New(Options{Store: p, Credentials: creds, Authority: s.authority, HistoryPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, ok := next.state.pending["persist-pending"]; !ok {
		t.Fatal("待登记未恢复")
	}
	next.history.Close()
	b, err := next.state.register(api.RegisterInput{ID: "persist-pending", ProviderID: "official"})
	if err != nil || b.ProviderID != "official" {
		t.Fatalf("JSON成功不应因投影失败回滚: %v", err)
	}
	if !next.historyDegraded.Load() {
		t.Fatal("索引故障未报告")
	}
	provider, err := next.state.resolve("persist-pending")
	if err != nil || provider != "official" {
		t.Fatal("索引失败影响绑定")
	}
	if _, err = next.state.resolve("another-unknown"); err == nil || !strings.Contains(err.Error(), "STORAGE_WRITE_FAILED") {
		t.Fatal("待登记写入失败必须拒绝而非套用默认")
	}
}
