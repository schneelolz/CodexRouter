package router

import (
	"codexrouter/internal/api"
	"fmt"
	"io"
	"net/http"
	"time"
)

type activityWriter struct {
	http.ResponseWriter
	diag        *requestDiagnostic // 当前请求的脱敏连接跟踪。
	started     time.Time          // 入口开始时间，耗时不包含记录落盘。
	provider    string             // 实际解析出的供应商。
	status      int
	code        string               // 固定错误类别，不保存原始上游错误。
	stage       string               // 错误发生阶段。
	observation *responseObservation // 仅记录观察到的元数据。
	failed      bool                 // 客户端写入或刷新失败不能被归为HTTP正常。
}

func (w *activityWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *activityWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(p)
	if n != len(p) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.failed = true
	}
	return n, err
}
func (w *activityWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (s *Server) beginActivity(r *http.Request) int {
	thread := r.Header.Get("Thread-Id")
	if !validID(thread) {
		thread = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received++
	s.activityStarted[s.received] = time.Now()
	if id, err := s.history.Begin(r.Method+" "+r.URL.Path, thread); err == nil {
		s.activityIDs[s.received] = id
	} else {
		s.historyDegraded.Store(true)
	}
	s.activities = append([]api.RequestActivity{{ID: s.received, Time: time.Now().UTC().Format(time.RFC3339), Entry: r.Method + " " + r.URL.Path, ThreadID: thread, Phase: "received", Result: "处理中", Outcome: "processing", RequestType: "unknown", Metrics: api.RequestMetrics{KnownFields: []string{}}, Observation: api.RequestObservation{TerminalResult: "unknown", MetricsStatus: "pending"}}}, s.activities...)
	if len(s.activities) > 100 {
		s.activities = s.activities[:100]
	}
	return s.received
}
func (s *Server) activityProvider(id int, provider string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stored, ok := s.activityIDs[id]; ok {
		if err := s.history.Provider(stored, provider); err != nil {
			s.historyDegraded.Store(true)
		}
	}
	for i := range s.activities {
		if s.activities[i].ID == id {
			s.activities[i].ProviderID = provider
			return
		}
	}
}
func (s *Server) endActivity(id int, tracked *activityWriter, interrupted bool) {
	item := api.RequestActivity{Status: tracked.status, Phase: "finished", Outcome: "unconfirmed", RequestType: "unknown", Result: fmt.Sprintf("HTTP_%d", tracked.status), Metrics: api.RequestMetrics{KnownFields: []string{}}}
	if tracked.status < 200 || tracked.status >= 300 {
		item.Outcome = "error"
		item.ErrorStage = "upstream"
	}
	if interrupted {
		item.Outcome = "error"
		item.Result = "STREAM_INTERRUPTED"
		item.ErrorStage = "stream"
	}
	if tracked.status == 0 {
		item.Outcome = "error"
		item.Result = "NO_RESPONSE"
		item.ErrorStage = "routing"
	}
	if tracked.code != "" {
		item.Outcome = "error"
		item.Result = tracked.code
		item.ErrorStage = tracked.stage
	}
	if tracked.observation != nil {
		tracked.observation.finish()
		// 观察只在写入并刷新成功后接收字节；终止送达后的客户端关闭不撤销生成完成。
		if tracked.code == "REQUEST_CANCELED" && !tracked.failed && tracked.status >= 200 && tracked.status < 300 && tracked.observation.terminal == "RESPONSE_COMPLETED" && !tracked.observation.lost {
			item.Outcome = "unconfirmed"
			item.ErrorStage = ""
		}
		tracked.observation.apply(&item)
	}
	item.Observation.TransportResult = tracked.code
	if item.Observation.TransportResult == "" {
		item.Observation.TransportResult = "EOF"
		if interrupted {
			item.Observation.TransportResult = "STREAM_INTERRUPTED"
		}
	}
	if item.Observation.TerminalResult == "" {
		item.Observation.TerminalResult = "unknown"
		item.Observation.MetricsStatus = "not_reported"
	}
	elapsed := max(0, int(time.Since(tracked.started).Milliseconds()))
	if tracked.provider != "" {
		if item.Outcome == "unconfirmed" && item.RequestType != "auxiliary" {
			s.recordStatus(tracked.provider, item.Result, "unverified")
		} else {
			s.record(tracked.provider, item.Result, item.Outcome == "success" || (item.RequestType == "auxiliary" && item.Status >= 200 && item.Status < 300 && !interrupted))
		}
	}
	if tracked.diag != nil {
		tracked.diag.finish(item, tracked)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item.ElapsedMS = elapsed
	if stored, ok := s.activityIDs[id]; ok {
		item.ID = stored
		if err := s.history.FinishActivity(item); err != nil {
			s.historyDegraded.Store(true)
		}
	}
	delete(s.activityIDs, id)
	delete(s.activityStarted, id)
	for i := range s.activities {
		if s.activities[i].ID == id {
			prior := s.activities[i]
			item.ID = prior.ID
			item.Time = prior.Time
			item.Entry = prior.Entry
			item.ThreadID = prior.ThreadID
			item.ProviderID = prior.ProviderID
			s.activities[i] = item
			return
		}
	}
}
