package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func compressedFixture(t *testing.T, body []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(body, nil)
}

func TestDecodeRequestEvidenceBoundsAndEncoding(t *testing.T) {
	plain := []byte(`{"input":[{"role":"user","content":"合成压缩测试"}]}`)
	compressed := compressedFixture(t, plain)
	corrupt := bytes.Clone(compressed)
	corrupt[len(corrupt)-1] ^= 0xff
	for _, test := range []struct {
		name string
		body []byte
		enc  []string
		ok   bool
	}{
		{"plain", plain, nil, true},
		{"identity", plain, []string{"identity"}, true},
		{"zstd", compressed, []string{"zstd"}, true},
		{"case-and-space", compressed, []string{" ZsTd "}, true},
		{"truncated", compressed[:len(compressed)-2], []string{"zstd"}, false},
		{"checksum", corrupt, []string{"zstd"}, false},
		{"invalid", plain, []string{"zstd"}, false},
		{"unsupported", plain, []string{"gzip"}, false},
		{"stacked", compressed, []string{"zstd, identity"}, false},
		{"duplicate", compressed, []string{"zstd", "zstd"}, false},
		{"oversized-window", []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0x80, 1, 0, 0}, []string{"zstd"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := decodeRequestEvidence(test.body, test.enc)
			if ok != test.ok || (ok && !bytes.Equal(got, plain)) {
				t.Fatalf("解码边界错误: ok=%v, decoded bytes=%d", ok, len(got))
			}
		})
	}
	for _, size := range []int{maxEvidenceBody, maxEvidenceBody + 1} {
		t.Run(fmt.Sprintf("decoded-size-%d", size), func(t *testing.T) {
			body := bytes.Repeat([]byte{'x'}, size)
			got, ok := decodeRequestEvidence(compressedFixture(t, body), []string{"zstd"})
			if ok != (size <= maxEvidenceBody) || (ok && !bytes.Equal(got, body)) {
				t.Fatalf("解压大小限制失效: input=%d ok=%v", size, ok)
			}
		})
	}
	if _, ok := decodeRequestEvidence(make([]byte, maxEvidenceBody+1), nil); ok {
		t.Fatal("原始正文超过上限仍被接受")
	}
}

func TestCompressedNewAndBoundRequestsPreserveWireBytesAndIdentity(t *testing.T) {
	fresh := []byte(`{"input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"user","content":"合成新请求"}]}`)
	history := []byte(`{"input":[{"role":"assistant","content":"合成历史"},{"role":"user","content":"合成后续"}]}`)
	for _, route := range []string{BuiltinOfficialID, BuiltinCompanyID} {
		t.Run(route, func(t *testing.T) {
			first, second := compressedFixture(t, fresh), compressedFixture(t, history)
			var calls atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				want := first
				if call == 2 {
					want = second
				}
				got, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(got, want) || r.Header.Get("Content-Encoding") != "zstd" {
					t.Error("压缩字节或编码声明被改写")
				}
				if route == BuiltinCompanyID {
					if r.Header.Get("Authorization") != "Bearer synthetic-company" || r.Header.Get("Chatgpt-Account-Id") != "" || r.Header.Get("Thread-Id") != "" {
						t.Error("压缩请求泄露官方身份或未用公司凭据")
					}
				} else if r.Header.Get("Authorization") != "Bearer synthetic-official" || r.Header.Get("Thread-Id") != "compressed-session" {
					t.Error("官方请求身份未保留")
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer up.Close()
			s, host := liveRouter(t, up.URL)
			if err := s.creds.Set("company", "synthetic-company"); err != nil {
				t.Fatal(err)
			}
			cfg := s.state.config()
			cfg.DefaultProviderID = route
			if err := s.state.saveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			for _, body := range [][]byte{first, second} {
				r := inferenceRequest(t, host.URL, "compressed-session", "new")
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
				r.Header.Set("Content-Encoding", "zstd")
				r.Header.Set("Authorization", "Bearer synthetic-official")
				r.Header.Set("Chatgpt-Account-Id", "synthetic-account")
				resp, err := host.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("有效压缩会话失败: %d", resp.StatusCode)
				}
			}
			bindings := s.state.sessions().Bindings
			if calls.Load() != 2 || len(bindings) != 1 || bindings[0].ProviderID != route {
				t.Fatal("压缩会话没有保持唯一正确绑定")
			}
		})
	}
}

func TestUnprovenCompressedRequestsNeverReachUpstream(t *testing.T) {
	fresh := []byte(`{"input":[{"role":"user","content":"合成请求"}]}`)
	compressed := compressedFixture(t, fresh)
	corrupt := bytes.Clone(compressed)
	corrupt[len(corrupt)-1] ^= 0xff
	for _, test := range []struct {
		name string
		body []byte
		enc  []string
	}{
		{"history", compressedFixture(t, []byte(`{"input":[{"role":"assistant","content":"合成历史"},{"role":"user","content":"合成请求"}]}`)), []string{"zstd"}},
		{"previous-response", compressedFixture(t, []byte(`{"previous_response_id":"synthetic-response","input":[{"role":"user"}]}`)), []string{"zstd"}},
		{"truncated", compressed[:len(compressed)-2], []string{"zstd"}},
		{"checksum", corrupt, []string{"zstd"}},
		{"missing-encoding", compressed, nil},
		{"unsupported", fresh, []string{"br"}},
		{"stacked", fresh, []string{"zstd, gzip"}},
		{"duplicate", compressed, []string{"zstd", "identity"}},
		{"too-large", compressedFixture(t, []byte(`{"input":[{"role":"user","content":"`+strings.Repeat("a", maxEvidenceBody)+`"}]}`)), []string{"zstd"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer up.Close()
			s, host := liveRouter(t, up.URL)
			cfg := s.state.config()
			cfg.DefaultProviderID = BuiltinCompanyID
			if err := s.state.saveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			r := inferenceRequest(t, host.URL, "unproven-compressed", "new")
			r.Body = io.NopCloser(bytes.NewReader(test.body))
			r.ContentLength = int64(len(test.body))
			r.Header["Content-Encoding"] = test.enc
			resp, err := host.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			sessions := s.state.sessions()
			if resp.StatusCode != http.StatusConflict || calls.Load() != 0 || len(sessions.Bindings) != 0 || len(sessions.Pending) != 1 {
				t.Fatalf("未证实请求被错误绑定或转发: status=%d calls=%d", resp.StatusCode, calls.Load())
			}
		})
	}
}

func TestEncodedResponsePreservesBytesAndEncodingHeaders(t *testing.T) {
	encoded := compressedFixture(t, []byte("event: synthetic\ndata: {}\n\n"))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("Content-Encoding", "zstd")
		w.Header().Add("Content-Encoding", "identity")
		_, _ = w.Write(encoded)
	}))
	defer up.Close()
	_, host := liveRouter(t, up.URL)
	resp, err := host.Client().Do(inferenceRequest(t, host.URL, "encoded-response", "new"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, encoded) || !reflect.DeepEqual(resp.Header.Values("Content-Encoding"), []string{"zstd", "identity"}) {
		t.Fatal("上游响应编码头或原始字节丢失")
	}
}
