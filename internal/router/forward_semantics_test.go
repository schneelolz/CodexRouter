package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestOfficialForwardPreservesProtocolHeadersAndBodyFraming(t *testing.T) {
	plain := []byte(`{"input":[{"role":"user","content":"synthetic"}],"future":{"retained":true}}`)
	for _, encoding := range []string{"identity", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			body := plain
			if encoding == "zstd" {
				body = compressedFixture(t, plain)
			}
			var calls atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				got, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(got, body) || r.ContentLength != int64(len(body)) || len(r.TransferEncoding) != 0 {
					t.Errorf("正文或长度变化: length=%d transfer=%v error=%v", r.ContentLength, r.TransferEncoding, err)
				}
				for name, want := range map[string]string{
					"User-Agent":                             "synthetic-codex-client/0.166.0",
					"X-Client-Request-Id":                    "synthetic-request",
					"X-Codex-Window-Id":                      "synthetic-window",
					"X-Openai-Internal-Codex-Responses-Lite": "true",
					"Content-Encoding":                       encoding,
				} {
					if r.Header.Get(name) != want {
						t.Errorf("协议头未保留: %s", name)
					}
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Local-Secret") != "" {
					t.Error("透传了本地或未知敏感头")
				}
				w.Header().Add("X-Codex-Turn-State", "synthetic-state-one")
				w.Header().Add("X-Codex-Turn-State", "synthetic-state-two")
				writeSyntheticCodexSSE(w, calls.Load())
			}))
			defer up.Close()
			_, host := liveRouter(t, up.URL)
			// 同一线程的首次证据读取及后续直接转发都必须保留长度。
			for range 2 {
				r := inferenceRequest(t, host.URL, "semantic-thread", "new")
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
				for name, value := range map[string]string{
					"User-Agent": "synthetic-codex-client/0.166.0", "X-Client-Request-Id": "synthetic-request",
					"X-Codex-Window-Id": "synthetic-window", "X-Openai-Internal-Codex-Responses-Lite": "true",
					"Content-Encoding": encoding, "Cookie": "synthetic=private", "X-Forwarded-For": "synthetic",
					"X-Local-Secret": "synthetic",
				} {
					r.Header.Set(name, value)
				}
				resp, err := host.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || !bytes.Contains(got, []byte("response.completed")) {
					t.Fatalf("SSE 未完成: status=%d error=%v", resp.StatusCode, err)
				}
				if !reflect.DeepEqual(resp.Header.Values("X-Codex-Turn-State"), []string{"synthetic-state-one", "synthetic-state-two"}) {
					t.Error("响应的多值 turn-state 未保留")
				}
			}
			if calls.Load() != 2 {
				t.Fatal("请求被重放或丢失")
			}
		})
	}
}

func TestGatewayFailureMessageRetainsCodeWithoutReplay(t *testing.T) {
	for _, mode := range []string{"disconnect", "header-timeout"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if mode == "header-timeout" {
					<-r.Context().Done()
					return
				}
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
			}))
			defer up.Close()
			_, host := liveRouter(t, up.URL)
			resp, err := host.Client().Do(inferenceRequest(t, host.URL, "gateway-thread", "new"))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var result struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			want := "UPSTREAM_CONNECTION_CLOSED"
			if mode == "header-timeout" {
				want = "UPSTREAM_TIMEOUT"
			}
			if resp.StatusCode != 502 || result.Error.Code != want || !bytes.Contains([]byte(result.Error.Message), []byte(want)) || calls.Load() != 1 {
				t.Fatalf("网关错误分类、可见消息或请求次数错误: status=%d code=%s calls=%d", resp.StatusCode, result.Error.Code, calls.Load())
			}
		})
	}
}

func TestFinishedClientBodyDoesNotLimitResponseHeaderWait(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			w.WriteHeader(200)
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	cfg := s.state.config()
	cfg.IdleTimeoutMS, cfg.HeaderTimeoutMS = 100, 2000
	if err := s.state.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		resp, err := host.Client().Do(inferenceRequest(t, host.URL, "header-wait-thread", "new"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("已完成正文的读取期限误取消响应头等待: %d", resp.StatusCode)
		}
	}
}
