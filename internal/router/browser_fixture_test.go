package router

import (
	"codexrouter/internal/daily"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// 此 fixture 只编入测试二进制，以合成状态提供真实 API/静态托管联调。
func TestBrowserFixtureServer(t *testing.T) {
	token := os.Getenv("CODEXROUTER_E2E_TOKEN")
	if token == "" {
		t.Skip("仅供隔离浏览器集成测试")
	}
	ui, err := filepath.Abs("../../frontend/dist")
	if err != nil {
		t.Fatal(err)
	}
	port := os.Getenv("CODEXROUTER_E2E_PORT")
	if port == "" {
		port = "19962"
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1024 || value > 65535 {
		t.Fatal("测试端口无效")
	}
	authority := net.JoinHostPort("127.0.0.1", port)
	home := t.TempDir()
	if err = os.WriteFile(filepath.Join(home, "config.toml"), []byte("# synthetic daily config\nmodel = 'synthetic-model'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dailyManager, err := daily.New(home, "http://"+authority, "synthetic-browser-fixture")
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := newSecurityServer(t, func(o *Options) { o.UIDir = ui; o.Authority = authority; o.Daily = dailyManager })
	s.bootstrap = digest(token)
	s.bootstrapExpiry = time.Now().Add(2 * time.Minute)
	// 控制入口只编入测试程序，且仅接受当前夹具的随机凭据。
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__fixture/bootstrap" || r.URL.Path == "/__fixture/expire" {
			if r.Method != "POST" || r.Host != authority || !equal(r.Header.Get("X-Fixture-Key"), token) {
				w.WriteHeader(403)
				return
			}
			if r.URL.Path == "/__fixture/expire" {
				s.mu.Lock()
				s.sessionExpiry = time.Now().Add(-time.Second)
				s.mu.Unlock()
				respond(w, 200, map[string]bool{"expired": true})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": s.Bootstrap()})
			return
		}
		s.ServeHTTP(w, r)
	})
	host := HTTPServer(authority, handler)
	timer := time.AfterFunc(230*time.Second, func() { _ = host.Close() })
	defer timer.Stop()
	if err = host.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
