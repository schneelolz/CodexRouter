package router

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codexrouter/internal/api"
	"github.com/klauspost/compress/zstd"
)

const observedComplete = `{"type":"response.completed","response":{"status":"completed","model":"synthetic-model","output":[{"type":"message"}],"usage":{"input_tokens":144746,"output_tokens":630,"total_tokens":145376,"input_tokens_details":{"cached_tokens":144400},"output_tokens_details":{"reasoning_tokens":400}}}}`

func observeFixture(body, contentType, encoding string, step int) api.RequestActivity {
	header := http.Header{"Content-Type": {contentType}}
	if encoding != "" {
		header.Set("Content-Encoding", encoding)
	}
	o := newResponseObservation(&http.Response{Header: header}, time.Now().Add(-25*time.Millisecond), "POST /responses")
	for len(body) > 0 {
		n := min(step, len(body))
		o.feed([]byte(body[:n]))
		body = body[n:]
	}
	o.finish()
	item := api.RequestActivity{Status: 200, Outcome: "unconfirmed"}
	o.apply(&item)
	return item
}

func TestObservationOutcomesAndPresence(t *testing.T) {
	for _, tc := range []struct{ name, body, contentType, outcome, result string }{
		{"success", "data: " + observedComplete + "\n\n", "text/event-stream", "success", "RESPONSE_COMPLETED"},
		{"failed200", "data: {\"type\":\"response.failed\",\"response\":{\"usage\":{\"input_tokens\":13}}}\n\n", "text/event-stream", "error", "RESPONSE_FAILED"},
		{"incomplete", "data: {\"type\":\"response.incomplete\"}\n\n", "text/event-stream", "error", "RESPONSE_INCOMPLETE"},
		{"bareError", "event: error\ndata: {\"error\":{\"message\":\"do-not-store-secret\"}}\n\n", "text/event-stream", "error", "RESPONSE_FAILED"},
		{"authoritativeCompleted", "event: error\ndata: {\"error\":{}}\n\ndata: " + observedComplete + "\n\n", "text/event-stream", "success", "RESPONSE_COMPLETED"},
		{"failedCannotFlip", "data: {\"type\":\"response.failed\"}\n\ndata: " + observedComplete + "\n\n", "text/event-stream", "error", "RESPONSE_FAILED"},
		{"missingTerminal", "data: {\"type\":\"response.created\"}\n\ndata: [DONE]\n\n", "text/event-stream", "error", "MISSING_TERMINAL"},
		{"truncatedTerminal", "data: " + observedComplete + "\n", "text/event-stream", "unconfirmed", "OBSERVATION_UNAVAILABLE"},
		{"malformed", "data: malformed\n\ndata: " + observedComplete + "\n\n", "text/event-stream", "unconfirmed", "OBSERVATION_UNAVAILABLE"},
		{"empty", "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n", "text/event-stream", "error", "EMPTY_RESPONSE"},
		{"sync", `{"status":"completed","output":[{}],"usage":{"input_tokens":0,"output_tokens":0}}`, "application/json", "success", "RESPONSE_COMPLETED"},
		{"compact", `{"object":"response.compaction","output":[{}]}`, "application/json", "success", "RESPONSE_COMPLETED"},
		{"syncUnknown", `{}`, "application/json", "unconfirmed", "RESULT_UNCONFIRMED"},
		{"unsupported", "opaque bytes", "application/octet-stream", "unconfirmed", "OBSERVATION_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := observeFixture(tc.body, tc.contentType, "", 7)
			if item.Outcome != tc.outcome || item.Result != tc.result {
				t.Fatalf("%+v", item)
			}
			if tc.name == "failed200" && item.Metrics.InputTokens != 13 {
				t.Fatal("失败事件的真实用量丢失")
			}
			if tc.name == "sync" && len(item.Metrics.KnownFields) != 4 {
				t.Fatalf("0与缺失未区分: %+v", item.Metrics)
			}
		})
	}
}

func TestObservationChunkFramingUsageAndTTFT(t *testing.T) {
	body := ": heartbeat\r\n\r\ndata: {\"type\":\"response.created\"}\r\n\r\n" +
		"event: response.output_text.delta\r\ndata: {\"delta\":\"中文\"}\r\n\r\n" +
		"event: response.completed\r\ndata: " + observedComplete[:40] + "\r\ndata: " + observedComplete[40:] + "\r\n\r\n"
	// 多行data只在JSON允许的空白边界分割。
	body = strings.ReplaceAll(body, observedComplete[:40]+"\r\ndata: "+observedComplete[40:], `{"type":"response.completed",`+"\r\ndata: "+strings.TrimPrefix(observedComplete, `{"type":"response.completed",`))
	for _, step := range []int{1, 2, 17, len(body)} {
		item := observeFixture(body, "text/event-stream", "identity", step)
		m := item.Metrics
		if item.Outcome != "success" || m.InputTokens != 144746 || m.CacheReadTokens != 144400 || m.TotalTokens != 145376 || m.ReasoningTokens != 400 || m.FirstTokenMS < 25 {
			t.Fatalf("step=%d %+v", step, item)
		}
	}
	for _, event := range []string{"response.reasoning_text.delta", "response.function_call_arguments.delta", "response.refusal.delta"} {
		item := observeFixture("data: {\"type\":\""+event+"\",\"delta\":\"x\"}\n\ndata: "+observedComplete+"\n\n", "text/event-stream", "", 3)
		if item.Metrics.FirstTokenMS < 25 {
			t.Fatalf("%s没有记录首字", event)
		}
	}
	item := observeFixture("data: {\"type\":\"response.created\"}\n\ndata: "+observedComplete+"\n\n", "text/event-stream", "", 7)
	for _, field := range item.Metrics.KnownFields {
		if field == "first_token_ms" {
			t.Fatal("把created或终止事件算作首字")
		}
	}
}

func TestObservationSnapshotsAreNotAddedOrZeroed(t *testing.T) {
	body := "data: " + observedComplete + "\n\ndata: " + observedComplete + "\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n"
	item := observeFixture(body, "text/event-stream", "", 11)
	if item.Metrics.TotalTokens != 145376 || item.Metrics.InputTokens != 144746 || item.Metrics.CacheReadTokens != 144400 {
		t.Fatalf("重复累加或零占位覆盖: %+v", item)
	}
	item = observeFixture(`{"status":"completed","output":[{}],"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":90}}}`, "application/json", "", 3)
	if item.Metrics.TotalTokens != 110 {
		t.Fatal("重复计入缓存")
	}
	item = observeFixture("data: "+observedComplete+"\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n", "text/event-stream", "", 3)
	if item.Metrics.TotalTokens != 2 || item.Metrics.CacheReadTokens != 0 {
		t.Fatalf("完整新快照沿用了旧缓存: %+v", item)
	}
}

func TestObservationCompressionAndBounds(t *testing.T) {
	body := "data: " + observedComplete + "\n\n"
	var gz bytes.Buffer
	writer := gzip.NewWriter(&gz)
	_, _ = writer.Write([]byte(body))
	_ = writer.Close()
	zs, _ := zstd.NewWriter(nil)
	encoded := zs.EncodeAll([]byte(body), nil)
	zs.Close()
	for encoding, compressed := range map[string][]byte{"gzip": gz.Bytes(), "zstd": encoded} {
		item := observeFixture(string(compressed), "text/event-stream", encoding, 3)
		if item.Outcome != "success" || item.Metrics.TotalTokens != 145376 {
			t.Fatalf("%s: %+v", encoding, item)
		}
		item = observeFixture(string(compressed[:len(compressed)-3]), "text/event-stream", encoding, 3)
		if item.Outcome == "success" {
			t.Fatalf("截断压缩流被当成成功: %s", encoding)
		}
	}
	item := observeFixture("data: "+strings.Repeat("x", observationFrameLimit+50)+"\n\ndata: "+observedComplete+"\n\n", "text/event-stream", "", 32768)
	if item.Outcome != "unconfirmed" || item.Metrics.TotalTokens != 145376 {
		t.Fatalf("大事件必须降级且能继续采集: %+v", item)
	}
	item = observeFixture(strings.Repeat(" ", observationFrameLimit+1), "application/json", "", 32768)
	if item.Outcome != "unconfirmed" {
		t.Fatal("大JSON未降级")
	}
	for _, raw := range []string{"-1", "1.2", "null", `"2"`, "9007199254740992"} {
		item = observeFixture(fmt.Sprintf(`{"status":"completed","output":[{}],"usage":{"input_tokens":%s}}`, raw), "application/json", "", 9)
		if len(item.Metrics.KnownFields) != 1 || item.Metrics.KnownFields[0] != "protocol_completed_ms" {
			t.Fatalf("非法数值被接受 %s", raw)
		}
	}
}

func TestObservedForwardPreservesWireBytesAndPersistsSemanticFailure(t *testing.T) {
	body := "data: {\"type\":\"response.failed\",\"response\":{\"model\":\"synthetic-model\",\"usage\":{\"input_tokens\":123}}}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	resp, err := host.Client().Do(inferenceRequest(t, host.URL, "observed-failed-session", "new"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(got) != body || resp.StatusCode != 200 {
		t.Fatalf("转发被改写: %q %v", got, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		page, err := s.history.Requests(20, "", "error")
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 1 {
			item := page.Items[0]
			if item.Result != "RESPONSE_FAILED" || item.Metrics.InputTokens != 123 || item.Model != "synthetic-model" {
				t.Fatalf("%+v", item)
			}
			s.mu.Lock()
			diagnostic := s.upstreams[BuiltinOfficialID]
			s.mu.Unlock()
			if diagnostic.Result != "failure" || diagnostic.Code != "RESPONSE_FAILED" {
				t.Fatal("诊断仍把200内失败当作成功", diagnostic)
			}
			success, _ := s.history.Requests(20, "", "success")
			if success.Total != 0 {
				t.Fatal("失败串入成功")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("记录未完成")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestObservedCompressedSuccessPreservesWireAndSeparatesAuxiliary(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			body := []byte("data: " + observedComplete + "\n\n")
			switch encoding {
			case "gzip":
				var compressed bytes.Buffer
				gz := gzip.NewWriter(&compressed)
				_, _ = gz.Write(body)
				_ = gz.Close()
				body = compressed.Bytes()
			case "zstd":
				zs, _ := zstd.NewWriter(nil)
				body = zs.EncodeAll(body, nil)
				zs.Close()
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"data":[]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", encoding)
				_, _ = w.Write(body)
			}))
			defer up.Close()
			s, host := liveRouter(t, up.URL)
			// 本用例验证编码字节，避免100ms故障探针期限在全量负载下干扰断言。
			cfg := s.state.config()
			cfg.ConnectTimeoutMS = 3000
			cfg.HeaderTimeoutMS = 3000
			cfg.IdleTimeoutMS = 3000
			cfg.WriteTimeoutMS = 3000
			if err := s.state.saveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			request := inferenceRequest(t, host.URL, "observed-compressed-session", "new")
			request.Header.Set("Accept-Encoding", encoding)
			resp, err := host.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || !bytes.Equal(got, body) || resp.Header.Get("Content-Encoding") != encoding {
				t.Fatalf("编码字节校验失败 status=%d encoding=%q expected_encoding=%q got_bytes=%d expected_bytes=%d error=%v", resp.StatusCode, resp.Header.Get("Content-Encoding"), encoding, len(got), len(body), err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				page, _ := s.history.Requests(20, "", "success")
				if page.Total == 1 {
					if page.Items[0].Metrics.TotalTokens != 145376 {
						t.Fatal("采集失败")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("未写入成功记录")
				}
				time.Sleep(time.Millisecond)
			}
			resp, err = host.Client().Get(host.URL + "/models")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			deadline = time.Now().Add(time.Second)
			for {
				page, _ := s.history.Requests(20, "", "unconfirmed")
				if page.Total == 1 {
					if page.Items[0].RequestType != "auxiliary" {
						t.Fatal("辅助请求未标明类型")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("辅助请求分类错误")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestObservedIdleTimeoutPersistsFixedCategory(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	resp, err := host.Client().Do(inferenceRequest(t, host.URL, "observed-idle-session", "new"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatal("超时伪装成正常EOF")
	}
	deadline := time.Now().Add(time.Second)
	for {
		page, _ := s.history.Requests(20, "", "error")
		if page.Total == 1 {
			if page.Items[0].Result != "STREAM_IDLE_TIMEOUT" || page.Items[0].ErrorStage != "stream" {
				t.Fatalf("%+v", page.Items[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("超时结果未持久化")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUnconfirmedObservationKeepsUpstreamUnverified(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	resp, err := host.Client().Do(inferenceRequest(t, host.URL, "observed-unknown-session", "new"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	deadline := time.Now().Add(time.Second)
	for {
		page, _ := s.history.Requests(20, "", "unconfirmed")
		if page.Total == 1 {
			s.mu.Lock()
			status := s.upstreams[BuiltinOfficialID]
			s.mu.Unlock()
			if status.Result != "unverified" {
				t.Fatal("未确认不应冒充上游故障", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("未确认记录未写入")
		}
		time.Sleep(time.Millisecond)
	}
}
