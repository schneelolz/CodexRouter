package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codexrouter/internal/api"
)

func liveRouter(t *testing.T, upstream string) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("settings-page"), 0600); err != nil {
		t.Fatal(err)
	}
	s, _, _ := newSecurityServer(t, func(o *Options) { o.UIDir = dir })
	for _, id := range []string{BuiltinOfficialID, BuiltinCompanyID} {
		if _, err := s.state.updateProvider(id, api.ProviderUpdateInput{Name: "合成线路", BaseURL: upstream, Model: ""}); err != nil {
			t.Fatal(err)
		}
	}
	c := s.state.config()
	c.ConnectTimeoutMS = 100
	c.HeaderTimeoutMS = 100
	c.IdleTimeoutMS = 100
	c.WriteTimeoutMS = 100
	if err := s.state.saveConfig(c); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s)
	s.authority = ts.Listener.Addr().String()
	s.origins = map[string]bool{"http://" + s.authority: true}
	ts.Start()
	t.Cleanup(ts.Close)
	return s, ts
}

func inferenceRequest(t *testing.T, base, id, history string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, base+"/responses", strings.NewReader(`{"stream":true,"input":[{"role":"user","content":"合成请求"}],"unknown":{"preserve":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Thread-Id", id)
	r.Header.Set("Session-Id", id)
	if history != "" {
		r.Header.Set("X-Codex-Turn-Metadata", fmt.Sprintf(`{"thread_id":%q,"session_id":%q,"history_initialization":%q}`, id, id, history))
	}
	return r
}

func TestStreamingPreservesBytesAndDeliversBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	first := []byte("event: 未知事件\ndata: {\"中文\":\"第一段\"}\n\n")
	tail := []byte("data: " + strings.Repeat("终", 100000) + "\n\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"unknown":{"preserve":true}`)) {
			t.Error("未知请求字段丢失")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(first[:13])
		w.(http.Flusher).Flush()
		_, _ = w.Write(first[13:])
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write(tail)
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	cfg := s.state.config()
	cfg.IdleTimeoutMS = 1000
	if err := s.state.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	resp, err := host.Client().Do(inferenceRequest(t, host.URL, "stream-session", "new"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make([]byte, len(first))
	_, err = io.ReadFull(resp.Body, got)
	close(release)
	if err != nil || !bytes.Equal(got, first) {
		t.Fatalf("首块非增量或被改写: %v", err)
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(rest, tail) {
		t.Fatalf("大响应/中文边界被改写: %v", err)
	}
}

func TestCompanyIdentityIsolationRedirectAndNoReplay(t *testing.T) {
	var companyCalls, redirectCalls atomic.Int64
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectCalls.Add(1) }))
	defer redirect.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		companyCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-company" {
			t.Error("未使用公司合成凭据")
		}
		for _, name := range []string{"Cookie", "Chatgpt-Account-Id", "Openai-Organization", "Openai-Project", "Thread-Id", "Session-Id", "X-Codex-Turn-Metadata", "X-Forwarded-For", "Originator", "X-Client-Request-Id", "X-Codex-Window-Id", "X-Openai-Internal-Codex-Responses-Lite"} {
			if r.Header.Get(name) != "" {
				t.Errorf("公司收到官方/本地身份头 %s", name)
			}
		}
		w.Header().Set("Location", redirect.URL+"/leak")
		w.WriteHeader(307)
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	_ = s.creds.Set("company", "synthetic-company")
	cfg := s.state.config()
	cfg.DefaultProviderID = BuiltinCompanyID
	if err := s.state.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	r := inferenceRequest(t, host.URL, "company-session", "new")
	r.Header.Set("Authorization", "Bearer synthetic-official")
	r.Header.Set("Cookie", "synthetic=private")
	r.Header.Set("Chatgpt-Account-Id", "synthetic-account")
	r.Header.Set("Openai-Organization", "synthetic-org")
	r.Header.Set("Originator", "synthetic-client")
	r.Header.Set("X-Client-Request-Id", "synthetic-request")
	r.Header.Set("X-Codex-Window-Id", "synthetic-window")
	r.Header.Set("X-Openai-Internal-Codex-Responses-Lite", "true")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 307 || companyCalls.Load() != 1 || redirectCalls.Load() != 0 {
		t.Fatal("发生重放或跟随重定向")
	}
	var resets atomic.Int64
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resets.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer broken.Close()
	_, second := liveRouter(t, broken.URL)
	resp, err = second.Client().Do(inferenceRequest(t, second.URL, "broken-session", "new"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 || resets.Load() != 1 {
		t.Fatal("生成请求失败后发生自动重放")
	}
}

func TestUnknownSessionAndPersistenceFailureNeverReachUpstream(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	resp, err := host.Client().Do(inferenceRequest(t, host.URL, "unknown-history", "cold_resume"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 || calls.Load() != 0 {
		t.Fatal("未知历史被自动转发")
	}
	p := s.state.store.(*testPersistence)
	p.setWriteError(fmt.Errorf("synthetic storage failure"))
	resp, err = host.Client().Do(inferenceRequest(t, host.URL, "unsaved-new", "new"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 || calls.Load() != 0 {
		t.Fatal("未持久绑定被转发")
	}
}

func TestCancelShutdownConcurrencyAndWriteStall(t *testing.T) {
	started := make(chan struct{}, 4)
	ended := make(chan struct{}, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: start\n\n"))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
		ended <- struct{}{}
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	cfg := s.state.config()
	cfg.MaxConcurrent = 1
	cfg.IdleTimeoutMS = 10000
	if err := s.state.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := inferenceRequest(t, host.URL, "cancel-session", "new").WithContext(ctx)
	resp, err := host.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	limited, err := host.Client().Do(inferenceRequest(t, host.URL, "limit-session", "new"))
	if err != nil {
		t.Fatal(err)
	}
	limited.Body.Close()
	if limited.StatusCode != 429 {
		t.Fatal("并发边界未生效")
	}
	cancel()
	resp.Body.Close()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("客户端取消未传播上游")
	}
	for s.active.Load() != 0 {
		time.Sleep(time.Millisecond)
	}
	resp, err = host.Client().Do(inferenceRequest(t, host.URL, "shutdown-session", "new"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	s.Close()
	resp.Body.Close()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("退出未取消剩余流")
	}

	// 原始 TCP 客户端只上传请求，停止读取，验证写停滞上限释放转发任务。
	flood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for i := 0; i < 4096; i++ {
			if _, e := w.Write(chunk); e != nil {
				return
			}
		}
	}))
	defer flood.Close()
	slow, slowHost := liveRouter(t, flood.URL)
	conn, err := net.Dial("tcp", strings.TrimPrefix(slowHost.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "POST /responses HTTP/1.1\r\nHost: %s\r\nThread-Id: slow-reader\r\nX-Codex-Turn-Metadata: {\"thread_id\":\"slow-reader\",\"history_initialization\":\"new\"}\r\nContent-Length: 2\r\nContent-Type: application/json\r\n\r\n{}", slow.authority)
	time.Sleep(300 * time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for slow.active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if slow.active.Load() != 0 {
		t.Fatal("慢客户端写停滞后仍保留活动任务")
	}
}

func TestNetworkFailuresRemainLocalAndRecover(t *testing.T) {
	tlsUp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer tlsUp.Close()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	closed.Close()
	for _, test := range []struct{ base, code string }{{"http://" + address, "UPSTREAM_UNREACHABLE"}, {tlsUp.URL, "UPSTREAM_TLS_FAILED"}, {"https://codexrouter-does-not-exist.invalid", "UPSTREAM_DNS_FAILED"}} {
		s, host := liveRouter(t, test.base)
		// 此处验证错误类型而非超时速度；留出Windows证书初始化和race调度时间。
		cfg := s.state.config()
		cfg.ConnectTimeoutMS, cfg.HeaderTimeoutMS, cfg.IdleTimeoutMS, cfg.WriteTimeoutMS = 3000, 3000, 3000, 3000
		if err := s.state.saveConfig(cfg); err != nil {
			t.Fatal(err)
		}
		if test.code == "UPSTREAM_DNS_FAILED" {
			tr := s.client(BuiltinOfficialID, s.state.config()).Transport.(*http.Transport)
			tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
				return nil, &net.DNSError{Err: "synthetic NXDOMAIN", Name: "synthetic.invalid", IsNotFound: true}
			}
		}
		resp, e := host.Client().Do(inferenceRequest(t, host.URL, "network-failure", "new"))
		if e != nil {
			t.Fatal(e)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 502 || !bytes.Contains(data, []byte(test.code)) {
			t.Errorf("网络错误分类不符，期望 %s: %s", test.code, data)
		}
		for _, path := range []string{"/healthz", "/"} {
			res, e := host.Client().Get(host.URL + path)
			if e != nil {
				t.Fatal(e)
			}
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatal("上游故障影响本地服务")
			}
		}
		if s.active.Load() != 0 {
			t.Fatal("故障后未释放任务")
		}
	}
}

func TestFaultRecovery100Cycles(t *testing.T) {
	var mode, calls, connections atomic.Int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		switch mode.Load() {
		case 1:
			w.WriteHeader(503)
		case 2:
			<-r.Context().Done()
		case 3:
			_, _ = w.Write([]byte("data: partial\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case 4:
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		default:
			_, _ = w.Write([]byte("data: recovered\n\n"))
		}
	}))
	up.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
		if s == http.StateClosed || s == http.StateHijacked {
			connections.Add(-1)
		}
	}
	up.Start()
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	cfg := s.state.config()
	// 100轮断连同时有5ms健康探测；正常恢复不应受100ms连接/写期限的调度噪声影响。
	cfg.ConnectTimeoutMS = 1000
	cfg.HeaderTimeoutMS = 1000
	cfg.WriteTimeoutMS = 1000
	cfg.IdleTimeoutMS = 200
	if err := s.state.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	probeDone := make(chan struct{})
	var probeFailures atomic.Int64
	go func() {
		defer close(probeDone)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				for _, path := range []string{"/healthz", "/"} {
					r, e := host.Client().Get(host.URL + path)
					if e != nil {
						probeFailures.Add(1)
						continue
					}
					_, _ = io.Copy(io.Discard, r.Body)
					r.Body.Close()
					if r.StatusCode != 200 {
						probeFailures.Add(1)
					}
				}
			}
		}
	}()
	defer func() { close(done); <-probeDone }()
	baseline := runtime.NumGoroutine()
	var mem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mem)
	initialHeap := mem.HeapAlloc
	for i := 0; i < 100; i++ {
		mode.Store(int64(i%4 + 1))
		resp, e := host.Client().Do(inferenceRequest(t, host.URL, "fault-cycles", "new"))
		if e == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if e != nil {
			t.Fatalf("本地转发入口失效: %v", e)
		}
		mode.Store(0)
		resp, e = host.Client().Do(inferenceRequest(t, host.URL, "fault-cycles", "new"))
		if e != nil {
			t.Fatal(e)
		}
		data, re := io.ReadAll(resp.Body)
		resp.Body.Close()
		if re != nil || string(data) != "data: recovered\n\n" || resp.StatusCode != 200 {
			t.Fatalf("循环%d无需重启恢复失败: status=%d body=%q err=%v", i, resp.StatusCode, data, re)
		}
		if (i+1)%20 == 0 {
			runtime.GC()
			runtime.ReadMemStats(&mem)
			t.Logf("batch=%d cycles=%d heap_alloc=%d goroutines=%d active=%d upstream_connections=%d", (i+1)/20, i+1, mem.HeapAlloc, runtime.NumGoroutine(), s.active.Load(), connections.Load())
		}
	}
	s.mu.Lock()
	for _, client := range s.clients {
		client.CloseIdleConnections()
	}
	s.mu.Unlock()
	host.Client().CloseIdleConnections()
	deadline := time.Now().Add(2 * time.Second)
	for (s.active.Load() != 0 || connections.Load() != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	runtime.GC()
	runtime.ReadMemStats(&mem)
	if calls.Load() != 200 {
		t.Fatalf("物理请求数%d，发生重放或回退", calls.Load())
	}
	if probeFailures.Load() != 0 || s.active.Load() != 0 || connections.Load() != 0 {
		t.Fatalf("本地可用性或资源边界失败: probes=%d active=%d connections=%d", probeFailures.Load(), s.active.Load(), connections.Load())
	}
	if mem.HeapAlloc > initialHeap+(16<<20) || runtime.NumGoroutine() > baseline+12 {
		t.Fatal("故障循环后资源未回落到测试边界")
	}
	t.Logf("cycles=100 requests=%d probe_failures=%d final_heap=%d baseline_heap=%d final_goroutines=%d baseline_goroutines=%d", calls.Load(), probeFailures.Load(), mem.HeapAlloc, initialHeap, runtime.NumGoroutine(), baseline)
}
