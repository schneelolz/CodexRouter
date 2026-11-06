package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestObservationUsesArrivalInsteadOfParsingTime(t *testing.T) {
	started := time.Now().Add(-time.Second)
	o := newResponseObservation(&http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}}, started, "POST /responses")
	o.feedAt([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"), started.Add(100*time.Millisecond))
	o.feedAt([]byte("data: "+observedComplete+"\n\n"), started.Add(200*time.Millisecond))
	o.finish()
	if o.metrics.FirstTokenMS != 100 || o.metrics.ProtocolCompletedMS != 200 {
		t.Fatalf("解析时间污染到达计时 %+v", o.metrics)
	}
}

func TestForwardTimingComparedWithDirectSSE(t *testing.T) {
	first := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"合成\"}\n\n"
	second := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"持续输出\"}\n\n"
	terminal := "data: " + observedComplete + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range []string{first, second, terminal} {
			time.Sleep(20 * time.Millisecond)
			_, _ = io.WriteString(w, part)
			w.(http.Flusher).Flush()
		}
		// 完成事件已发送，上游继续占用连接；路由器仍必须原样转发且保留两种耗时。
		time.Sleep(140 * time.Millisecond)
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	cfg := s.state.config()
	cfg.IdleTimeoutMS = 1000
	// 全套race并行运行时不能沿用liveRouter的100ms响应头探针超时。
	cfg.HeaderTimeoutMS = 3000
	cfg.ConnectTimeoutMS = 1000
	if err := s.state.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	type timing struct{ first, terminal, closed time.Duration }
	measure := func(base, id string) timing {
		start := time.Now()
		r := inferenceRequest(t, base, id, "new")
		resp, err := host.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		prefix := make([]byte, len(first))
		if _, err = io.ReadFull(resp.Body, prefix); err != nil || string(prefix) != first {
			t.Fatalf("首块未及时或原样交付: status=%d prefix=%q error=%v", resp.StatusCode, prefix, err)
		}
		result := timing{first: time.Since(start)}
		next := make([]byte, len(second)+len(terminal))
		if _, err = io.ReadFull(resp.Body, next); err != nil || string(next) != second+terminal {
			t.Fatal("后续块未原样交付", err)
		}
		result.terminal = time.Since(start)
		rest, err := io.ReadAll(resp.Body)
		if err != nil || len(rest) != 0 {
			t.Fatal("尾流被改写", err)
		}
		result.closed = time.Since(start)
		if result.closed-result.terminal < 100*time.Millisecond {
			t.Fatal("未覆盖延迟EOF")
		}
		return result
	}
	var direct, proxy timing
	for i := range 3 {
		d := measure(up.URL, "direct-timing")
		p := measure(host.URL, fmt.Sprintf("proxy-timing-%d", i))
		direct.first += d.first
		direct.terminal += d.terminal
		direct.closed += d.closed
		proxy.first += p.first
		proxy.terminal += p.terminal
		proxy.closed += p.closed
	}
	t.Logf("三次平均：直连首块=%s 终止=%s 连接结束=%s；路由首块=%s 终止=%s 连接结束=%s；首块额外开销=%s", direct.first/3, direct.terminal/3, direct.closed/3, proxy.first/3, proxy.terminal/3, proxy.closed/3, (proxy.first-direct.first)/3)
	deadline := time.Now().Add(time.Second)
	for {
		page, err := s.history.Requests(20, "", "success")
		if err != nil {
			t.Fatal(err)
		}
		if page.Total == 3 {
			for _, item := range page.Items {
				if item.ElapsedMS-item.Metrics.ProtocolCompletedMS < 100 || item.Metrics.FirstTokenMS > item.Metrics.ProtocolCompletedMS {
					t.Fatalf("协议/收尾未分开 %+v", item)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("时序记录未完成")
		}
		time.Sleep(time.Millisecond)
	}
}

func BenchmarkStreamingObservation(b *testing.B) {
	body := []byte(strings.Repeat("data: {\"type\":\"response.output_text.delta\",\"delta\":\"合成\"}\n\n", 100) + "data: " + observedComplete + "\n\n")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		o := newResponseObservation(&http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}}, time.Now(), "POST /responses")
		o.feed(body)
		o.finish()
	}
}
