package router

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codexrouter/internal/api"
	"github.com/klauspost/compress/zstd"
)

func TestTerminalDeliveredBeforeClientCancel(t *testing.T) {
	for _, tc := range []struct{ name, terminal, outcome, result, encoding string }{
		{"completed", observedComplete, "success", "RESPONSE_COMPLETED", "identity"},
		{"completed-gzip", observedComplete, "success", "RESPONSE_COMPLETED", "gzip"},
		{"completed-zstd", observedComplete, "success", "RESPONSE_COMPLETED", "zstd"},
		{"failed", `{"type":"response.failed","response":{"usage":{"input_tokens":13}}}`, "error", "RESPONSE_FAILED", "identity"},
		{"incomplete", `{"type":"response.incomplete"}`, "error", "RESPONSE_INCOMPLETE", "identity"},
		{"midstream", `{"type":"response.created"}`, "error", "REQUEST_CANCELED", "identity"},
		{"truncated", observedComplete[:len(observedComplete)-5], "error", "REQUEST_CANCELED", "identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\ndata: " + tc.terminal + "\n\n")
			switch tc.encoding {
			case "gzip":
				var buffer bytes.Buffer
				writer := gzip.NewWriter(&buffer)
				_, _ = writer.Write(body)
				_ = writer.Close()
				body = buffer.Bytes()
			case "zstd":
				writer, _ := zstd.NewWriter(nil)
				body = writer.EncodeAll(body, nil)
				writer.Close()
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", tc.encoding)
				_, _ = w.Write(body)
				_ = http.NewResponseController(w).Flush()
				// 完成事件后故意不关闭连接，模拟收到终止事件即关闭的客户端。
				<-r.Context().Done()
			}))
			defer up.Close()
			s, host := liveRouter(t, up.URL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := inferenceRequest(t, host.URL, "terminal-cancel-test", "new").WithContext(ctx)
			request.Header.Set("Accept-Encoding", tc.encoding)
			resp, err := host.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(body))
			_, err = io.ReadFull(resp.Body, got)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("终止字节未原样送达: %v", err)
			}
			cancel()
			resp.Body.Close()
			item := waitFinishedRequest(t, s)
			if item.Outcome != tc.outcome || item.Result != tc.result {
				t.Fatalf("终止后取消覆盖协议结果: outcome=%s result=%s", item.Outcome, item.Result)
			}
			if item.Observation.TransportResult != "REQUEST_CANCELED" || item.Observation.MetricsStatus == "legacy" || item.Metrics.FirstTokenMS < 0 {
				t.Fatalf("传输与协议事实未独立保存: %+v", item)
			}
			if tc.outcome == "success" && (item.Observation.TerminalResult != "RESPONSE_COMPLETED" || item.Metrics.OutputTokens != 630 || item.Observation.MetricsStatus != "available") {
				t.Fatalf("完成结果或用量丢失: %+v", item)
			}
		})
	}
}

func TestDeliveredTerminalDoesNotHideWriteFailure(t *testing.T) {
	s, _ := liveRouter(t, "http://127.0.0.1:1")
	id := s.beginActivity(httptest.NewRequest("POST", "/responses", nil))
	o := newResponseObservation(&http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}}, time.Now(), "POST /responses")
	o.feed([]byte("data: " + observedComplete + "\n\n"))
	s.endActivity(id, &activityWriter{started: time.Now(), status: 200, observation: o, code: "CLIENT_WRITE_FAILED", stage: "client", failed: true}, true)
	item := waitFinishedRequest(t, s)
	if item.Outcome != "error" || item.Result != "CLIENT_WRITE_FAILED" || item.Observation.TerminalResult != "RESPONSE_COMPLETED" || item.Observation.TransportResult != "CLIENT_WRITE_FAILED" {
		t.Fatalf("已见终止掩盖写入失败: %+v", item)
	}
}

func waitFinishedRequest(t *testing.T, s *Server) api.RequestActivity {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		page, err := s.history.Requests(20, "", "all")
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 1 && page.Items[0].Phase == "finished" {
			return page.Items[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("请求没有完成落盘")
	return api.RequestActivity{}
}
