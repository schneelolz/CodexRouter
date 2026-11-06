package router

import (
	"codexrouter/internal/daily"
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
	host := HTTPServer(authority, s)
	timer := time.AfterFunc(115*time.Second, func() { _ = host.Close() })
	defer timer.Stop()
	if err = host.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
