package router

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codexrouter/internal/api"
	"codexrouter/internal/diaglog"
)

var diagnosticID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type requestDiagnostic struct {
	mu                         sync.Mutex // httptrace 回调可能由多个拨号协程调用。
	s                          *Server
	id                         int
	started                    time.Time
	dns, tls, writing, waiting time.Time
	connecting                 map[string]time.Time // 只在内存关联并行拨号，地址不进入日志。
	fields                     diaglog.Fields
	sent                       atomic.Int64
	read, written              int64 // 仅转发处理协程更新。
	first, last                time.Time
}

func newRequestDiagnostic(s *Server, id int, started time.Time, r *http.Request, cfg api.Config) *requestDiagnostic {
	s.mu.Lock()
	historyID := s.activityIDs[id]
	s.mu.Unlock()
	d := &requestDiagnostic{s: s, id: id, started: started, connecting: make(map[string]time.Time), fields: diaglog.Fields{
		"method": safeMethod(r.Method), "entry": r.URL.Path, "history_id": historyID,
		"dns_ms": nil, "connect_ms": nil, "tls_ms": nil, "send_ms": nil, "header_wait_ms": nil,
		"dns_result": "unknown", "connect_result": "unknown", "tls_result": "unknown", "send_result": "unknown", "header_result": "unknown",
		"reused": nil, "was_idle": nil, "connection_idle_ms": nil, "http_version": "unknown",
		"timeouts_ms": map[string]int{"connect": cfg.ConnectTimeoutMS, "headers": cfg.HeaderTimeoutMS, "read_idle": cfg.IdleTimeoutMS, "write": cfg.WriteTimeoutMS},
	}}
	d.emit("request_start", nil)
	return d
}

func durationMS(start time.Time) any {
	if start.IsZero() {
		return nil
	}
	return float64(time.Since(start).Microseconds()) / 1000
}

func diagnosticError(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "eof"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connection_reset"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return "tls_certificate"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "other"
}

func (d *requestDiagnostic) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { d.mu.Lock(); d.dns = time.Now(); d.mu.Unlock() },
		DNSDone: func(v httptrace.DNSDoneInfo) {
			d.mu.Lock()
			d.fields["dns_ms"] = durationMS(d.dns)
			d.fields["dns_result"] = diagnosticError(v.Err)
			d.mu.Unlock()
		},
		ConnectStart: func(network, address string) {
			d.mu.Lock()
			d.connecting[network+"\x00"+address] = time.Now()
			d.mu.Unlock()
		},
		ConnectDone: func(network, address string, err error) {
			d.mu.Lock()
			key := network + "\x00" + address
			began := d.connecting[key]
			delete(d.connecting, key)
			// IPv4/IPv6 并行拨号时保留成功连接的耗时，失败尝试不能覆盖成功。
			if err == nil || d.fields["connect_result"] != "ok" {
				d.fields["connect_ms"] = durationMS(began)
				d.fields["connect_result"] = diagnosticError(err)
			}
			d.mu.Unlock()
		},
		TLSHandshakeStart: func() { d.mu.Lock(); d.tls = time.Now(); d.mu.Unlock() },
		TLSHandshakeDone: func(v tls.ConnectionState, err error) {
			d.mu.Lock()
			d.fields["tls_ms"] = durationMS(d.tls)
			d.fields["tls_result"] = diagnosticError(err)
			if v.NegotiatedProtocol != "" {
				d.fields["alpn"] = v.NegotiatedProtocol
			}
			d.mu.Unlock()
		},
		GotConn: func(v httptrace.GotConnInfo) {
			d.mu.Lock()
			d.fields["reused"] = v.Reused
			d.fields["was_idle"] = v.WasIdle
			d.fields["connection_idle_ms"] = float64(v.IdleTime.Microseconds()) / 1000
			d.writing = time.Now()
			d.mu.Unlock()
		},
		WroteRequest: func(v httptrace.WroteRequestInfo) {
			d.mu.Lock()
			d.fields["send_ms"] = durationMS(d.writing)
			d.fields["send_result"] = diagnosticError(v.Err)
			if v.Err == nil {
				d.waiting = time.Now()
			}
			d.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			d.mu.Lock()
			d.fields["header_wait_ms"] = durationMS(d.waiting)
			d.fields["response_first_byte_ms"] = durationMS(d.started)
			d.fields["header_result"] = "first_byte"
			d.mu.Unlock()
		},
	}
}

func (d *requestDiagnostic) emit(event string, extra diaglog.Fields) {
	if d.s.log == nil {
		return
	}
	d.mu.Lock()
	fields := make(diaglog.Fields, len(d.fields)+len(extra)+2)
	for k, v := range d.fields {
		fields[k] = v
	}
	fields["request_id"] = d.id
	for k, v := range extra {
		fields[k] = v
	}
	d.mu.Unlock()
	d.s.log.Emit(event, fields)
}

func (d *requestDiagnostic) headers(resp *http.Response) {
	d.mu.Lock()
	d.fields["http_version"] = resp.Proto
	d.fields["upstream_status"] = resp.StatusCode
	d.fields["header_result"] = "ok"
	d.fields["response_encoding"] = safeEncoding(resp.Header.Values("Content-Encoding"))
	if id := resp.Header.Get("X-Request-Id"); diagnosticID.MatchString(id) {
		d.fields["upstream_request_id"] = id
	}
	d.mu.Unlock()
	d.emit("upstream_headers", nil)
}

func safeEncoding(v []string) string {
	if len(v) == 0 {
		return "identity"
	}
	if len(v) != 1 {
		return "other"
	}
	switch strings.ToLower(strings.TrimSpace(v[0])) {
	case "", "identity":
		return "identity"
	case "gzip":
		return "gzip"
	case "zstd":
		return "zstd"
	case "br":
		return "br"
	}
	return "other"
}

func (d *requestDiagnostic) received(n int, at time.Time) {
	if n <= 0 {
		return
	}
	d.read += int64(n)
	if d.first.IsZero() {
		d.first = at
	}
	d.last = at
}

func stamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (d *requestDiagnostic) finish(item api.RequestActivity, tracked *activityWriter) {
	source := "upstream"
	if tracked.code != "" {
		source = "router"
	}
	if tracked.code == "STREAM_INTERRUPTED" {
		source = "upstream_transport"
	}
	if tracked.code == "REQUEST_CANCELED" || tracked.code == "CLIENT_WRITE_FAILED" {
		source = "client"
	}
	if tracked.code == "ROUTER_SHUTDOWN" {
		source = "process"
	}
	transportSource := source
	if item.Result == item.Observation.TerminalResult && item.Observation.TerminalResult != "unknown" && item.Observation.TerminalResult != "" {
		source = "upstream_protocol"
	}
	lastIdle := any(nil)
	if !d.last.IsZero() {
		lastIdle = durationMS(d.last)
	}
	d.emit("request_end", diaglog.Fields{"provider_id": tracked.provider,
		"status":                 item.Status,
		"result":                 item.Result,
		"outcome":                item.Outcome,
		"error_stage":            item.ErrorStage,
		"result_source":          source,
		"transport_source":       transportSource,
		"transport_result":       item.Observation.TransportResult,
		"elapsed_ms":             durationMS(d.started),
		"request_body_bytes":     d.sent.Load(),
		"response_read_bytes":    d.read,
		"response_written_bytes": d.written,
		"first_body_at":          stamp(d.first),
		"last_body_at":           stamp(d.last),
		"last_read_idle_ms":      lastIdle,
		"terminal_result":        item.Observation.TerminalResult,
		"upstream_error_type":    tracked.observationError()})
}

func (w *activityWriter) observationError() string {
	if w.observation == nil || w.observation.errorType == "" {
		return "unknown"
	}
	return w.observation.errorType
}

type diagnosticBody struct {
	io.ReadCloser
	d *requestDiagnostic
}

func (b *diagnosticBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	b.d.sent.Add(int64(n))
	return n, e
}

func (s *Server) authEvent(event, reason string) { s.log.Emit(event, diaglog.Fields{"reason": reason}) }

// DiagnosticHTTPWriter 返回不会泄露原始 net/http 错误的日志适配器。
func DiagnosticHTTPWriter(logger *diaglog.Logger) io.Writer { return diagnosticHTTPWriter{log: logger} }

type diagnosticHTTPWriter struct{ log *diaglog.Logger }

func (w diagnosticHTTPWriter) Write(p []byte) (int, error) {
	// net/http 的原始诊断可能包含 URL、客户端输入或堆栈，只有固定类别落盘。
	class := "http_server_error"
	if strings.Contains(string(p), "panic serving") {
		class = "handler_panic"
	}
	w.log.Emit("http_server_error", diaglog.Fields{"class": class})
	return len(p), nil
}

func safeMethod(method string) string {
	switch method {
	case "GET", "POST", "HEAD", "PUT", "DELETE", "OPTIONS", "PATCH":
		return method
	}
	return "other"
}
func (d *requestDiagnostic) failed(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fields["network_error"] = diagnosticError(err)
	if !d.waiting.IsZero() && d.fields["header_result"] == "unknown" {
		d.fields["header_wait_ms"] = durationMS(d.waiting)
		d.fields["header_result"] = diagnosticError(err)
	}
}
