package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codexrouter/internal/api"
	"codexrouter/internal/platform"
	"codexrouter/internal/store"
)

type testPersistence struct {
	mu       sync.Mutex
	data     []byte
	readErr  error
	writeErr error
	writes   int
}

func (p *testPersistence) Read(_ string, out any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.readErr != nil {
		return p.readErr
	}
	if len(p.data) == 0 {
		return store.ErrNotFound
	}
	return json.Unmarshal(p.data, out)
}

func (p *testPersistence) Write(_ string, value any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes++
	if p.writeErr != nil {
		return p.writeErr
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	p.data = bytes.Clone(data)
	return nil
}

func (p *testPersistence) setWriteError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeErr = err
}

func (p *testPersistence) snapshot() ([]byte, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return bytes.Clone(p.data), p.writes
}

type testCredentials struct {
	mu        sync.Mutex
	values    map[string]string
	getErr    error
	setErr    error
	deleteErr error
}

func (c *testCredentials) Get(name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getErr != nil {
		return "", c.getErr
	}
	value, ok := c.values[name]
	if !ok {
		return "", platform.ErrNotFound
	}
	return value, nil
}

func (c *testCredentials) Set(name, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.setErr != nil {
		return c.setErr
	}
	if c.values == nil {
		c.values = make(map[string]string)
	}
	c.values[name] = value
	return nil
}

func (c *testCredentials) Delete(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleteErr != nil {
		return c.deleteErr
	}
	delete(c.values, name)
	return nil
}

type securityRequestOptions struct {
	method      string
	path        string
	body        string
	host        string
	origin      string
	csrf        string
	cookie      *http.Cookie
	contentType string
	headers     map[string]string
}

func securityRequest(s *Server, in securityRequestOptions) *httptest.ResponseRecorder {
	if in.method == "" {
		in.method = http.MethodGet
	}
	if in.host == "" {
		in.host = s.authority
	}
	r := httptest.NewRequest(in.method, "http://"+s.authority+in.path, strings.NewReader(in.body))
	r.Host = in.host
	if in.origin != "" {
		r.Header.Set("Origin", in.origin)
	}
	if in.csrf != "" {
		r.Header.Set("X-CSRF-Token", in.csrf)
	}
	if in.cookie != nil {
		r.AddCookie(in.cookie)
	}
	if in.contentType != "" {
		r.Header.Set("Content-Type", in.contentType)
	} else if in.body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for name, value := range in.headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func newSecurityServer(t *testing.T, edit func(*Options)) (*Server, *testPersistence, *testCredentials) {
	t.Helper()
	p := &testPersistence{}
	c := &testCredentials{}
	o := Options{Store: p, Credentials: c, Authority: "127.0.0.1:18788"}
	if edit != nil {
		edit(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, p, c
}

func securityLogin(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	token := s.Bootstrap()
	data, _ := json.Marshal(api.AuthInput{Token: token})
	w := securityRequest(s, securityRequestOptions{method: http.MethodPost, path: "/api/v1/auth/exchange", body: string(data), origin: "http://" + s.authority})
	if w.Code != http.StatusOK {
		t.Fatalf("引导认证失败: %d %s", w.Code, w.Body.String())
	}
	var session api.AuthSession
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil || session.CSRFToken == "" {
		t.Fatal("缺少认证会话防伪值")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("引导认证未设置唯一会话 Cookie")
	}
	return cookies[0], session.CSRFToken
}

func TestManagementRequiresAuthenticationIncludingCapabilities(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	for _, path := range []string{"/api/v1/capabilities", "/api/v1/status", "/api/v1/config", "/api/v1/credentials", "/api/v1/sessions", "/api/v1/diagnostics", "/api/v1/auth/session"} {
		t.Run(path, func(t *testing.T) {
			w := securityRequest(s, securityRequestOptions{path: path})
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "AUTH_REQUIRED") {
				t.Fatalf("未认证管理读取未被阻止: %d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("管理接口不应开放 CORS")
			}
		})
	}
	if w := securityRequest(s, securityRequestOptions{path: "/healthz"}); w.Code != http.StatusOK {
		t.Fatal("无上游依赖的健康检查不可用")
	}
}

func TestBootstrapIsSingleUseExpiringAndSessionBound(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	oldToken := s.Bootstrap()
	token := s.Bootstrap()
	for _, test := range []struct {
		name   string
		token  string
		origin string
		status int
	}{
		{"缺少 Origin", token, "", http.StatusForbidden},
		{"错误引导值", "invalid", "http://" + s.authority, http.StatusUnauthorized},
		{"新引导替换旧值", oldToken, "http://" + s.authority, http.StatusUnauthorized},
		{"外站 Origin", token, "https://evil.example", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, _ := json.Marshal(api.AuthInput{Token: test.token})
			w := securityRequest(s, securityRequestOptions{method: http.MethodPost, path: "/api/v1/auth/exchange", body: string(data), origin: test.origin})
			if w.Code != test.status {
				t.Fatalf("引导保护返回 %d，期望 %d", w.Code, test.status)
			}
		})
	}
	data, _ := json.Marshal(api.AuthInput{Token: token})
	exchange := securityRequestOptions{method: http.MethodPost, path: "/api/v1/auth/exchange", body: string(data), origin: "http://" + s.authority}
	w := securityRequest(s, exchange)
	if w.Code != http.StatusOK {
		t.Fatalf("合法引导失败: %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/api/v1/" || cookies[0].MaxAge <= 0 {
		t.Fatal("管理 Cookie 缺少限定路径、HttpOnly、SameSite 或到期时间")
	}
	if strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), cookies[0].Value) {
		t.Fatal("响应正文不应回传引导值或会话 Cookie")
	}
	if replay := securityRequest(s, exchange); replay.Code != http.StatusUnauthorized {
		t.Fatal("同一引导值被重复使用")
	}
	oldCookie := cookies[0]
	newCookie, _ := securityLogin(t, s)
	if w := securityRequest(s, securityRequestOptions{path: "/api/v1/config", cookie: oldCookie}); w.Code != http.StatusUnauthorized {
		t.Fatal("新管理会话建立后旧会话仍有效")
	}
	if w := securityRequest(s, securityRequestOptions{path: "/api/v1/config", cookie: newCookie}); w.Code != http.StatusOK {
		t.Fatal("当前管理会话不可用")
	}
	s.mu.Lock()
	s.sessionExpiry = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if w := securityRequest(s, securityRequestOptions{path: "/api/v1/config", cookie: newCookie}); w.Code != http.StatusUnauthorized {
		t.Fatal("过期管理会话仍有效")
	}
	token = s.Bootstrap()
	s.mu.Lock()
	s.bootstrapExpiry = time.Now().Add(-time.Second)
	s.mu.Unlock()
	data, _ = json.Marshal(api.AuthInput{Token: token})
	if w := securityRequest(s, securityRequestOptions{method: http.MethodPost, path: "/api/v1/auth/exchange", body: string(data), origin: "http://" + s.authority}); w.Code != http.StatusUnauthorized {
		t.Fatal("过期引导值仍有效")
	}
}

func TestHostOriginCSRFAndForwardedHeaders(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	cookie, csrf := securityLogin(t, s)
	cfg := s.state.config()
	config, _ := json.Marshal(api.ConfigInput{DefaultProviderID: cfg.DefaultProviderID, ConnectTimeoutMS: cfg.ConnectTimeoutMS, HeaderTimeoutMS: cfg.HeaderTimeoutMS, IdleTimeoutMS: cfg.IdleTimeoutMS, WriteTimeoutMS: cfg.WriteTimeoutMS, MaxConcurrent: cfg.MaxConcurrent})
	for _, test := range []struct {
		name   string
		host   string
		origin string
		csrf   string
		status int
	}{
		{"错误 Host", "evil.example", "http://" + s.authority, csrf, http.StatusForbidden},
		{"近似 Host", "127.0.0.1:187880", "http://" + s.authority, csrf, http.StatusForbidden},
		{"外站 Origin", s.authority, "https://evil.example", csrf, http.StatusForbidden},
		{"null Origin", s.authority, "null", csrf, http.StatusForbidden},
		{"缺少 Origin", s.authority, "", csrf, http.StatusForbidden},
		{"缺少 CSRF", s.authority, "http://" + s.authority, "", http.StatusForbidden},
		{"错误 CSRF", s.authority, "http://" + s.authority, "invalid", http.StatusForbidden},
		{"同源写入", s.authority, "http://" + s.authority, csrf, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := securityRequest(s, securityRequestOptions{method: http.MethodPut, path: "/api/v1/config", body: string(config), host: test.host, origin: test.origin, csrf: test.csrf, cookie: cookie, headers: map[string]string{"Forwarded": "host=" + s.authority, "X-Forwarded-Host": s.authority, "X-Forwarded-Proto": "http"}})
			if w.Code != test.status {
				t.Fatalf("管理保护返回 %d，期望 %d: %s", w.Code, test.status, w.Body.String())
			}
		})
	}
	if w := securityRequest(s, securityRequestOptions{path: "/api/v1/config?token=must-not-use-query", cookie: cookie}); w.Code != http.StatusBadRequest {
		t.Fatal("管理接口接受查询参数")
	}
}

func TestDevelopmentOriginIsExplicitAndStillRequiresAuthentication(t *testing.T) {
	s, _, _ := newSecurityServer(t, func(o *Options) { o.DevOrigin = "http://127.0.0.1:18789" })
	if w := securityRequest(s, securityRequestOptions{path: "/api/v1/capabilities", origin: "http://127.0.0.1:18789"}); w.Code != http.StatusUnauthorized {
		t.Fatal("开发源绕过管理认证")
	}
	if w := securityRequest(s, securityRequestOptions{path: "/healthz", origin: "http://127.0.0.1:18790"}); w.Code != http.StatusForbidden {
		t.Fatal("开发端口允许列表被放宽")
	}
	for _, origin := range []string{"https://127.0.0.1:18789", "http://localhost:18789", "http://127.0.0.1:18789/path", "http://evil.example", "http://user@127.0.0.1:18789", "http://127.0.0.1:18789?x=1", "http://127.0.0.1:18789#fragment"} {
		created, err := New(Options{Store: &testPersistence{}, Credentials: &testCredentials{}, Authority: "127.0.0.1:18788", DevOrigin: origin})
		if created != nil {
			created.Close()
		}
		if err == nil {
			t.Errorf("接受未明确允许的开发源: %q", origin)
		}
	}
}

func TestCredentialsNeverReturnSecretsAndFailuresAreExplicit(t *testing.T) {
	s, p, creds := newSecurityServer(t, nil)
	cookie, csrf := securityLogin(t, s)
	const secret = "synthetic-company-key-never-return"
	w := securityRequest(s, securityRequestOptions{method: http.MethodPut, path: "/api/v1/credentials/company", body: `{"key":"` + secret + `"}`, origin: "http://" + s.authority, csrf: csrf, cookie: cookie})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"configured":true`) || strings.Contains(w.Body.String(), secret) {
		t.Fatal("凭据写入响应未限定为配置状态")
	}
	for _, path := range []string{"/api/v1/credentials", "/api/v1/providers", "/api/v1/config", "/api/v1/status", "/api/v1/diagnostics"} {
		w := securityRequest(s, securityRequestOptions{path: path, cookie: cookie})
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("凭据泄漏或管理读取失败: %s", path)
		}
	}
	data, _ := p.snapshot()
	if bytes.Contains(data, []byte(secret)) {
		t.Fatal("公司凭据进入 JSON 状态文件")
	}
	for _, test := range []struct {
		method string
		path   string
		set    func(error)
	}{
		{http.MethodGet, "/api/v1/providers", func(err error) { creds.getErr = err }},
		{http.MethodGet, "/api/v1/credentials", func(err error) { creds.getErr = err }},
		{http.MethodPut, "/api/v1/credentials/company", func(err error) { creds.setErr = err }},
		{http.MethodDelete, "/api/v1/credentials/company", func(err error) { creds.deleteErr = err }},
	} {
		creds.mu.Lock()
		test.set(errors.New("synthetic backend failure " + secret))
		creds.mu.Unlock()
		w := securityRequest(s, securityRequestOptions{method: test.method, path: test.path, body: `{"key":"replacement"}`, origin: "http://" + s.authority, csrf: csrf, cookie: cookie})
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "CREDENTIAL_UNAVAILABLE") || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("凭据后端失败未被安全报告: %d %s", w.Code, w.Body.String())
		}
		creds.mu.Lock()
		test.set(nil)
		creds.mu.Unlock()
	}
	if w := securityRequest(s, securityRequestOptions{method: http.MethodPut, path: "/api/v1/credentials/official", body: `{"key":"replacement"}`, origin: "http://" + s.authority, csrf: csrf, cookie: cookie}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "PROVIDER_NOT_API_KEY") {
		t.Fatal("官方转发供应商接受了 API 密钥")
	}
	w = securityRequest(s, securityRequestOptions{method: http.MethodDelete, path: "/api/v1/credentials/company", origin: "http://" + s.authority, csrf: csrf, cookie: cookie})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatal("凭据删除未返回未配置状态")
	}
	w = securityRequest(s, securityRequestOptions{path: "/api/v1/credentials", cookie: cookie})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"configured":false`) || strings.Contains(w.Body.String(), "official") {
		t.Fatal("凭据列表未限定为密钥供应商状态")
	}
}
func TestCredentialBackendFailurePreventsServerStartup(t *testing.T) {
	for _, creds := range []*testCredentials{{getErr: errors.New("unavailable")}, {setErr: errors.New("unavailable")}} {
		s, err := New(Options{Store: &testPersistence{}, Credentials: creds, Authority: "127.0.0.1:18788"})
		if s != nil {
			s.Close()
		}
		if err == nil {
			t.Fatal("控制凭据无法读写时仍启动服务")
		}
	}
}

func TestMalformedManagementJSONRejected(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	cookie, csrf := securityLogin(t, s)
	for _, test := range []struct {
		body        string
		contentType string
		status      int
	}{
		{`{"key":"synthetic","extra":true}`, "application/json", http.StatusBadRequest},
		{`{"key":"synthetic"} {}`, "application/json", http.StatusBadRequest},
		{`{"key":"synthetic"}`, "text/plain", http.StatusUnsupportedMediaType},
		{`{"key":"` + strings.Repeat("x", 65536) + `"}`, "application/json", http.StatusBadRequest},
		{`{"key":"line
break"}`, "application/json", http.StatusBadRequest},
		{`{"key":""}`, "application/json", http.StatusBadRequest},
	} {
		w := securityRequest(s, securityRequestOptions{method: http.MethodPut, path: "/api/v1/credentials/company", body: test.body, contentType: test.contentType, origin: "http://" + s.authority, csrf: csrf, cookie: cookie})
		if w.Code != test.status {
			t.Fatalf("非法 JSON/密钥返回 %d，期望 %d", w.Code, test.status)
		}
	}
}
func TestStaticIsolationTraversalAndNoAPIFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<main>synthetic-ui</main>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "asset.js"), []byte("synthetic-asset"), 0600); err != nil {
		t.Fatal(err)
	}
	s, _, _ := newSecurityServer(t, func(o *Options) { o.UIDir = dir })
	cookie, _ := securityLogin(t, s)
	for _, path := range []string{"/", "/index.html", "/asset.js"} {
		w := securityRequest(s, securityRequestOptions{path: path})
		if w.Code != http.StatusOK {
			t.Fatalf("静态资源不可用 %s: %d", path, w.Code)
		}
		if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("静态资源缺少安全响应头")
		}
	}
	for _, path := range []string{"/missing", "/../outside.txt", "/%2e%2e/outside.txt", "/..%5coutside.txt", "/.hidden", "/api/v1/missing", "/v1/responses"} {
		w := securityRequest(s, securityRequestOptions{path: path, cookie: cookie})
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "synthetic-ui") {
			t.Fatalf("非法资源/API 被静态页面掩盖: %s %d", path, w.Code)
		}
	}
	if w := securityRequest(s, securityRequestOptions{method: http.MethodPost, path: "/index.html", body: "{}"}); w.Code != http.StatusNotFound {
		t.Fatal("静态资源接受写入方法")
	}
	if err := os.WriteFile(filepath.Join(dir, "asset.js"), []byte("replacement-without-go-build"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := securityRequest(s, securityRequestOptions{path: "/asset.js"}); w.Code != http.StatusOK || w.Body.String() != "replacement-without-go-build" {
		t.Fatal("独立前端文件替换未生效")
	}
}

func TestStaticSymlinkCannotEscapeRoot(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside-sensitive-synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("ui"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.txt")); err != nil {
		t.Skipf("系统不允许创建符号链接，逃逸实测未执行: %v", err)
	}
	s, _, _ := newSecurityServer(t, func(o *Options) { o.UIDir = dir })
	w := securityRequest(s, securityRequestOptions{path: "/escape.txt"})
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "outside-sensitive-synthetic") {
		t.Fatal("符号链接可逃逸静态根目录")
	}
}

func TestDiagnosticsAreBoundedAndUpstreamStartsUnverified(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	cookie, _ := securityLogin(t, s)
	w := securityRequest(s, securityRequestOptions{path: "/api/v1/status", cookie: cookie})
	var status api.Status
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || len(status.Upstreams) != 2 {
		t.Fatal("状态接口未列出全部供应商")
	}
	for _, item := range status.Upstreams {
		if item.Result != "unverified" || item.CheckedAt != "" || item.Code != "" {
			t.Fatal("未发生上游请求时不应报告上游正常")
		}
	}
	for range 230 {
		s.record(BuiltinOfficialID, "SYNTHETIC_FAILURE", false)
	}
	w = securityRequest(s, securityRequestOptions{path: "/api/v1/diagnostics", cookie: cookie})
	var diagnostics api.Diagnostics
	if err := json.Unmarshal(w.Body.Bytes(), &diagnostics); err != nil || len(diagnostics.Items) != 200 || diagnostics.Items[0].ProviderID != BuiltinOfficialID {
		t.Fatal("近期诊断未遵守数量边界或缺少供应商标识")
	}
}
