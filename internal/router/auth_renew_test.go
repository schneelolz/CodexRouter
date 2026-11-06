package router

import (
	"net/http"
	"sync"
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
