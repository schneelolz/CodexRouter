package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRenewSessionWindowSecurityAndConcurrentTabs(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	cookie, csrf := securityLogin(t, s)
	options := securityRequestOptions{method: "POST", path: "/api/v1/auth/renew", cookie: cookie, csrf: csrf, origin: "http://" + s.authority}
	initial := s.sessionExpiry
	w := securityRequest(s, options)
	if w.Code != 200 || len(w.Result().Cookies()) != 0 || !s.sessionExpiry.Equal(initial) {
		t.Fatal("正常期限不应反复延长", w.Code)
	}
	s.sessionExpiry = time.Now().Add(59 * time.Minute)
	before := time.Now()
	w = securityRequest(s, options)
	cookies := w.Result().Cookies()
	if w.Code != 200 || len(cookies) != 1 || cookies[0].Value != cookie.Value || cookies[0].MaxAge != 28800 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/api/v1/" || s.sessionExpiry.Before(before.Add(8*time.Hour)) {
		t.Fatal("续期未同步安全Cookie与服务端期限", w.Code)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := securityRequest(s, options)
			if w.Code != 200 {
				t.Errorf("并发标签续期失败%d", w.Code)
			}
		}()
	}
	wg.Wait()
	for _, bad := range []securityRequestOptions{{method: "POST", path: options.path}, {method: "POST", path: options.path, cookie: cookie, origin: options.origin}, {method: "POST", path: options.path, cookie: cookie, csrf: csrf, origin: "https://evil.test"}} {
		w = securityRequest(s, bad)
		if w.Code != 401 && w.Code != 403 {
			t.Fatal("续期认证来源或CSRF可绕过", w.Code)
		}
	}
	s.sessionExpiry = time.Now().Add(-time.Second)
	w = securityRequest(s, options)
	if w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Fatal("失效凭据被续期")
	}
}

func TestExpiredManagementSessionDoesNotBlockInference(t *testing.T) {
	for _, provider := range []string{BuiltinOfficialID, BuiltinCompanyID} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				expected := "Bearer synthetic-client-token"
				if provider == BuiltinCompanyID {
					expected = "Bearer synthetic-provider-key"
				}
				if r.Header.Get("Authorization") != expected {
					t.Error("upstream credential mismatch")
				}
				if r.Header.Get("Cookie") != "" {
					t.Error("management cookie leaked upstream")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
			}))
			defer upstream.Close()
			s, host := liveRouter(t, upstream.URL)
			if provider == BuiltinCompanyID {
				if err := s.creds.Set(provider, "synthetic-provider-key"); err != nil {
					t.Fatal(err)
				}
			}
			config := s.state.config()
			config.DefaultProviderID = provider
			if err := s.state.saveConfig(config); err != nil {
				t.Fatal(err)
			}
			cookie, _ := securityLogin(t, s)
			s.mu.Lock()
			s.sessionExpiry = time.Now().Add(-time.Hour)
			s.mu.Unlock()
			if w := securityRequest(s, securityRequestOptions{path: "/api/v1/status", cookie: cookie}); w.Code != http.StatusUnauthorized {
				t.Fatal("expired management cookie accepted")
			}
			request := inferenceRequest(t, host.URL, "synthetic-expired-management-"+provider, "new")
			request.Header.Set("Authorization", "Bearer synthetic-client-token")
			request.AddCookie(cookie)
			response, err := host.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "response.completed") || calls.Load() != 1 {
				t.Fatal("management expiry blocked or replayed generation", err)
			}
			item := waitFinishedRequest(t, s)
			if item.Outcome != "success" {
				t.Fatal("forwarded generation not recorded as success", item.Result)
			}
		})
	}
}

func TestRequestPageParametersDoNotMixCursorOrSessionMode(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	cookie, _ := securityLogin(t, s)
	for _, path := range []string{"/api/v1/requests?page=1&cursor=", "/api/v1/requests?page=0", "/api/v1/requests?page=1000001", "/api/v1/requests?snapshot=x", "/api/v1/requests?page=1&page=2", "/api/v1/sessions/page?page=1", "/api/v1/sessions/page?snapshot=x"} {
		w := securityRequest(s, securityRequestOptions{path: path, cookie: cookie})
		if w.Code != 400 {
			t.Fatalf("参数未拒绝%s: %d", path, w.Code)
		}
	}
	w := securityRequest(s, securityRequestOptions{path: "/api/v1/requests?page=1&filter=success", cookie: cookie})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
