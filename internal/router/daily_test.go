package router

import (
	"bytes"
	"codexrouter/internal/api"
	"codexrouter/internal/daily"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dailyServer(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("# synthetic\nmodel='keep'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := daily.New(home, "http://127.0.0.1:18788", "synthetic-router-test")
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := newSecurityServer(t, func(o *Options) { o.Daily = m })
	return s, path
}
func TestDailyAPISecurityAndRestore(t *testing.T) {
	s, path := dailyServer(t)
	cookie, csrf := securityLogin(t, s)
	origin := "http://" + s.authority
	for _, entry := range []struct{ path, body string }{{"/api/v1/daily", ""}, {"/api/v1/daily/preview", `{"action":"route"}`}, {"/api/v1/daily/apply", `{"token":"synthetic"}`}, {"/api/v1/activity", ""}} {
		method := "POST"
		if entry.body == "" {
			method = "GET"
		}
		w := securityRequest(s, securityRequestOptions{method: method, path: entry.path, body: entry.body})
		if w.Code != 401 {
			t.Fatal("未拒绝匿名接口", entry.path, w.Code)
		}
		if method == "POST" {
			w = securityRequest(s, securityRequestOptions{method: method, path: entry.path, body: entry.body, cookie: cookie, origin: origin})
			if w.Code != 403 {
				t.Fatal("未拒绝缺少CSRF")
			}
		}
	}
	request := func(path, body string) []byte {
		t.Helper()
		w := securityRequest(s, securityRequestOptions{method: "POST", path: path, body: body, cookie: cookie, csrf: csrf, origin: origin})
		if w.Code != 200 {
			t.Fatalf("状态 %d %s", w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	for _, action := range []string{"route", "restore"} {
		raw := request("/api/v1/daily/preview", `{"action":"`+action+`"}`)
		var preview api.DailyPreview
		if err := json.Unmarshal(raw, &preview); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "model='keep'") {
			t.Fatal("预览泄漏配置正文")
		}
		body, _ := json.Marshal(api.DailyApply{Token: preview.Token})
		request("/api/v1/daily/apply", string(body))
	}
	final, _ := os.ReadFile(path)
	if string(final) != "# synthetic\nmodel='keep'\n" {
		t.Fatal("恢复改变其他配置")
	}
	w := securityRequest(s, securityRequestOptions{path: "/api/v1/daily", cookie: cookie})
	if w.Code != 200 {
		t.Fatal("状态不可读")
	}
	var doc contractDocument
	raw, err := os.ReadFile("../../contracts/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &doc) != nil {
		t.Fatal("契约损坏")
	}
	var value any
	_ = json.Unmarshal(w.Body.Bytes(), &value)
	if err = validateSchema(doc, doc.Components.Schemas["DailyStatus"], value, "daily"); err != nil {
		t.Fatal(err)
	}
}
func TestActivityActualRequestsBoundedAndNoSecrets(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	// 没有有效新建证据的真实入口请求会等待登记，仍计入实际收到。
	for i := 0; i < 105; i++ {
		r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"secret":"synthetic-body-secret"}`))
		r.Host = s.authority
		r.Header.Set("Thread-Id", "synthetic-activity")
		r.Header.Set("Authorization", "Bearer synthetic-header-secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 409 {
			t.Fatal("应等待登记", w.Code, w.Body.String())
		}
	}
	cookie, _ := securityLogin(t, s)
	w := securityRequest(s, securityRequestOptions{path: "/api/v1/activity", cookie: cookie})
	if w.Code != 200 {
		t.Fatal("读取失败")
	}
	if bytes.Contains(w.Body.Bytes(), []byte("synthetic-body-secret")) || bytes.Contains(w.Body.Bytes(), []byte("synthetic-header-secret")) {
		t.Fatal("活动记录泄漏")
	}
	var activity api.Activity
	_ = json.Unmarshal(w.Body.Bytes(), &activity)
	if activity.Received != 105 || len(activity.Items) != 100 || activity.Items[0].Result != "SESSION_REGISTRATION_REQUIRED" || activity.Items[0].ProviderID != "" {
		t.Fatal("活动统计错误")
	}
}
