package router

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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

	"codexrouter/internal/diaglog"
	"github.com/klauspost/compress/zstd"
)

func readDiagnostic(t *testing.T, dir string) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	var rows []map[string]any
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
			if len(line) == 0 || line[len(line)-1] != '\n' {
				continue
			}
			var v map[string]any
			if err := json.Unmarshal(line, &v); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, v)
		}
	}
	return rows
}

func TestDiagnosticTimeoutStreamAndUpstreamSources(t *testing.T) {
	for _, kind := range []string{"headers", "idle", "broken", "http502", "failed", "ok", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if kind == "headers" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Request-Id", "synthetic-upstream-42")
				if kind == "http502" {
					w.WriteHeader(502)
					_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"synthetic-private-response"}}`))
					return
				}
				if kind == "broken" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
					return
				}
				if kind == "idle" || kind == "cancel" {
					_, _ = w.Write([]byte(": initial\n\n"))
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if kind == "failed" {
					_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"server_error\",\"message\":\"synthetic-private-response\"}}}\n\n"))
					return
				}
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
			}))
			defer up.Close()
			s, host := liveRouter(t, up.URL)
			cfg := s.state.config()
			cfg.ConnectTimeoutMS = 3000
			cfg.HeaderTimeoutMS = 3000
			cfg.IdleTimeoutMS = 3000
			cfg.WriteTimeoutMS = 3000
			if kind == "headers" {
				cfg.HeaderTimeoutMS = 100
			}
			if kind == "idle" {
				cfg.IdleTimeoutMS = 100
			}
			if err := s.state.saveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			logger, err := diaglog.Open(dir, "test")
			if err != nil {
				t.Fatal(err)
			}
			s.log = logger
			req := inferenceRequest(t, host.URL, "diagnostic-session", "new")
			req.Header.Set("Authorization", "Bearer synthetic-private-token")
			req.Header.Set("Cookie", "secret=synthetic-private-cookie")
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			resp, err := host.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "cancel" {
				buf := make([]byte, 11)
				_, _ = io.ReadFull(resp.Body, buf)
				cancel()
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			waitFinishedRequest(t, s)
			logger.Close()
			rows := readDiagnostic(t, dir)
			var end map[string]any
			for _, row := range rows {
				if row["event"] == "request_end" {
					end = row["fields"].(map[string]any)
				}
			}
			if end == nil {
				t.Fatal("缺少结束记录", rows)
			}
			expect := map[string]string{"headers": "UPSTREAM_TIMEOUT", "idle": "STREAM_IDLE_TIMEOUT", "broken": "STREAM_INTERRUPTED", "http502": "HTTP_502", "failed": "RESPONSE_FAILED", "ok": "RESPONSE_COMPLETED", "cancel": "REQUEST_CANCELED"}[kind]
			if end["result"] != expect {
				t.Fatal("分类错误", end)
			}
			if end["request_id"] == nil || end["history_id"] == float64(0) || end["connect_ms"] == nil || end["reused"] != false {
				t.Fatal("关联或连接信息缺失", end)
			}
			if kind != "headers" && end["upstream_request_id"] != "synthetic-upstream-42" {
				t.Fatal("缺上游关联", end)
			}
			data, _ := json.Marshal(rows)
			for _, secret := range []string{"synthetic-private-token", "synthetic-private-cookie", "synthetic-private-response", up.URL} {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatal("日志泄露合成敏感值")
				}
			}
		})
	}
}

// TestDiagnosticSoak 使用显式环境开关执行真实时长隔离持续测试。
func TestDiagnosticSoak(t *testing.T) {
	value := os.Getenv("CODEXROUTER_SOAK_DURATION")
	if value == "" {
		t.Skip("仅显式持续验收")
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < time.Minute {
		t.Fatal("持续时长无效")
	}
	var calls, connections atomic.Int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n%19 == 0 {
			w.WriteHeader(502)
			return
		}
		if n%23 == 0 {
			_, _ = w.Write([]byte(": wait\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
	}))
	up.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
		if state == http.StateClosed || state == http.StateHijacked {
			connections.Add(-1)
		}
	}
	up.Start()
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	cfg := s.state.config()
	cfg.HeaderTimeoutMS = 2000
	cfg.ConnectTimeoutMS = 2000
	cfg.WriteTimeoutMS = 2000
	cfg.IdleTimeoutMS = 200
	if err := s.state.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("CODEXROUTER_SOAK_LOG_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	logger, err := diaglog.Open(dir, "soak")
	if err != nil {
		t.Fatal(err)
	}
	s.log = logger
	defer logger.Close()
	baseline := runtime.NumGoroutine()
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	heap := mem.HeapAlloc
	started := time.Now()
	nextReport := started.Add(time.Minute)
	count := 0
	for time.Since(started) < duration {
		resp, err := host.Client().Do(inferenceRequest(t, host.URL, "synthetic-soak", "new"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		count++
		resp, err = host.Client().Get(host.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal("健康检查失败")
		}
		if time.Now().After(nextReport) {
			runtime.ReadMemStats(&mem)
			t.Logf("elapsed=%s requests=%d active=%d conns=%d heap=%d goroutines=%d log=%+v", time.Since(started).Round(time.Second), count, s.active.Load(), connections.Load(), mem.HeapAlloc, runtime.NumGoroutine(), logger.Snapshot())
			nextReport = nextReport.Add(time.Minute)
		}
		time.Sleep(time.Second)
	}
	s.mu.Lock()
	for _, client := range s.clients {
		client.CloseIdleConnections()
	}
	s.mu.Unlock()
	host.Client().CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for (connections.Load() != 0 || s.active.Load() != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	logger.Close()
	runtime.GC()
	runtime.ReadMemStats(&mem)
	if calls.Load() != int64(count) || s.active.Load() != 0 || connections.Load() != 0 || runtime.NumGoroutine() > baseline+12 || mem.HeapAlloc > heap+(16<<20) || logger.Snapshot().Dropped != 0 || logger.Snapshot().WriteFailures != 0 {
		t.Fatal("资源或日志边界失败", calls.Load(), count, s.active.Load(), connections.Load(), runtime.NumGoroutine(), mem.HeapAlloc, logger.Snapshot())
	}
	t.Log(fmt.Sprintf("soak_complete duration=%s requests=%d active=0 connections=0 heap=%d goroutines=%d logs=%+v", time.Since(started), count, mem.HeapAlloc, runtime.NumGoroutine(), logger.Snapshot()))
}

func TestSafeDiagnosticValues(t *testing.T) {
	if safeEncoding([]string{"gzip"}) != "gzip" || safeEncoding([]string{"secret"}) != "other" || safeUpstreamErrorType("secret") != "unknown" {
		t.Fatal("未限制白名单")
	}
	if diagnosticID.MatchString(strings.Repeat("x", 129)) || diagnosticID.MatchString("secret\nvalue") {
		t.Fatal("上游关联号未限长")
	}
}

func TestDiagnosticHTTP2CompressionAndReuse(t *testing.T) {
	for _, protocol := range []string{"h1", "h2"} {
		for _, encoding := range []string{"identity", "gzip", "zstd"} {
			t.Run(protocol+"-"+encoding, func(t *testing.T) {
				payload := []byte("data: " + observedComplete + "\n\n")
				wire := payload
				if encoding == "gzip" {
					var b bytes.Buffer
					gz := gzip.NewWriter(&b)
					_, _ = gz.Write(payload)
					_ = gz.Close()
					wire = b.Bytes()
				}
				if encoding == "zstd" {
					zs, _ := zstd.NewWriter(nil)
					wire = zs.EncodeAll(payload, nil)
					zs.Close()
				}
				var calls atomic.Int64
				up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Content-Encoding", encoding)
					w.Header().Set("X-Request-Id", "unsafe\tvalue")
					_, _ = w.Write(wire)
				}))
				up.EnableHTTP2 = protocol == "h2"
				up.StartTLS()
				defer up.Close()
				s, host := liveRouter(t, up.URL)
				cfg := s.state.config()
				cfg.HeaderTimeoutMS = 3000
				cfg.ConnectTimeoutMS = 3000
				cfg.IdleTimeoutMS = 1000
				cfg.WriteTimeoutMS = 1000
				_ = s.state.saveConfig(cfg)
				// 只信任本测试服务器的证书，不关闭证书验证。
				transport := s.client(BuiltinOfficialID, cfg).Transport.(*http.Transport)
				transport.TLSClientConfig = up.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				dir := t.TempDir()
				logger, _ := diaglog.Open(dir, "test")
				s.log = logger
				for range 2 {
					req := inferenceRequest(t, host.URL, "diagnostic-reuse", "new")
					req.Header.Set("Accept-Encoding", encoding)
					resp, err := host.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || !bytes.Equal(got, wire) {
						t.Fatal("原字节发生改变", err)
					}
				}
				deadline := time.Now().Add(3 * time.Second)
				for {
					page, err := s.history.Requests(20, "", "all")
					if err != nil {
						t.Fatal(err)
					}
					finished := 0
					for _, item := range page.Items {
						if item.Phase == "finished" {
							finished++
						}
					}
					if finished == 2 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("两个请求没有完成收尾")
					}
					time.Sleep(time.Millisecond)
				}
				logger.Close()
				var ends []map[string]any
				for _, row := range readDiagnostic(t, dir) {
					if row["event"] == "request_end" {
						ends = append(ends, row["fields"].(map[string]any))
					}
				}
				want := "HTTP/1.1"
				if protocol == "h2" {
					want = "HTTP/2.0"
				}
				if len(ends) != 2 || calls.Load() != 2 || ends[0]["http_version"] != want || ends[1]["reused"] != true || ends[1]["connect_ms"] != nil || ends[0]["tls_ms"] == nil || ends[1]["response_encoding"] != encoding || ends[1]["result"] != "RESPONSE_COMPLETED" {
					t.Fatal("连接复用、协议或计数异常", ends, calls.Load())
				}
				for _, end := range ends {
					if end["upstream_request_id"] != nil {
						t.Fatal("非法上游编号被记录")
					}
				}
			})
		}
	}
}

func TestAuthDiagnosticsAndLoggerDegradation(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	dir := t.TempDir()
	logger, _ := diaglog.Open(dir, "test")
	s.log = logger
	securityRequest(s, securityRequestOptions{path: "/api/v1/status"})
	cookie, csrf := securityLogin(t, s)
	securityRequest(s, securityRequestOptions{method: "POST", path: "/api/v1/auth/renew", cookie: cookie, origin: "http://" + s.authority, csrf: "synthetic-invalid"})
	securityLogin(t, s)
	securityRequest(s, securityRequestOptions{path: "/api/v1/status", cookie: cookie})
	s.sessionExpiry = time.Now().Add(-time.Second)
	securityRequest(s, securityRequestOptions{path: "/api/v1/status", cookie: cookie})
	logger.Close()
	data, _ := json.Marshal(readDiagnostic(t, dir))
	for _, reason := range []string{"cookie_missing", "csrf_mismatch", "cookie_mismatch", "session_expired", "session_replaced", "bootstrap_exchanged"} {
		if !bytes.Contains(data, []byte(reason)) {
			t.Fatal("缺少认证事件", reason)
		}
	}
	for _, secret := range []string{cookie.Value, csrf, "synthetic-invalid"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("认证值泄露")
		}
	}
	blocked := filepath.Join(t.TempDir(), "blocked")
	_ = os.WriteFile(blocked, nil, 0600)
	bad, _ := diaglog.Open(blocked, "test")
	s.log = bad
	defer bad.Close()
	current, _ := securityLogin(t, s)
	w := securityRequest(s, securityRequestOptions{path: "/api/v1/diagnostics", cookie: current})
	if !strings.Contains(w.Body.String(), "LOG_WRITE_FAILED") {
		t.Fatal("缺少降级提示")
	}
	var diagnostics struct {
		Items []struct {
			ProviderID string `json:"provider_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &diagnostics); err != nil {
		t.Fatal(err)
	}
	for _, item := range diagnostics.Items {
		if item.ProviderID == "" {
			t.Fatal("诊断归属不符合现有契约")
		}
	}
}

func TestDiagnosticSlowClientWriteFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for range 8192 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	dir := t.TempDir()
	logger, err := diaglog.Open(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	s.log = logger
	conn, err := net.Dial("tcp", strings.TrimPrefix(host.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := inferenceRequest(t, host.URL, "diagnostic-slow-client", "new")
	if err := request.Write(conn); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var end map[string]any
	for time.Now().Before(deadline) {
		for _, row := range readDiagnostic(t, dir) {
			if row["event"] == "request_end" {
				end = row["fields"].(map[string]any)
			}
		}
		if end != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	logger.Close()
	if end == nil || end["result"] != "CLIENT_WRITE_FAILED" || end["result_source"] != "client" || s.active.Load() != 0 {
		t.Fatal("慢客户端未释放或缺少诊断", end)
	}
}

func TestDiagnosticConcurrentDialTiming(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	r := httptest.NewRequest("POST", "http://"+s.authority+"/responses", nil)
	d := newRequestDiagnostic(s, 1, time.Now(), r, s.state.config())
	trace := d.trace()
	trace.ConnectStart("tcp", "synthetic-v6")
	trace.ConnectStart("tcp", "synthetic-v4")
	trace.ConnectDone("tcp", "synthetic-v4", nil)
	trace.ConnectDone("tcp", "synthetic-v6", context.DeadlineExceeded)
	if d.fields["connect_result"] != "ok" || len(d.connecting) != 0 {
		t.Fatal("并行失败拨号覆盖成功连接", d.fields)
	}
}
