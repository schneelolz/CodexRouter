package router

import (
	"bytes"
	"codexrouter/internal/diaglog"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"syscall"
	"time"

	"codexrouter/internal/api"
)

type threadEvidence struct {
	ThreadID  string `json:"thread_id"`
	SessionID string `json:"session_id"`
	History   string `json:"history_initialization"`
	Parent    string `json:"forked_from_thread_id"`
}

// 新建标记在后续轮次也会重复；只有尚无生成历史的请求才可用于首次自动分配。
func hasFreshInput(body []byte) bool {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	for _, name := range []string{"previous_response_id", "conversation"} {
		if v := request[name]; len(v) > 0 && string(v) != "null" && string(v) != `""` {
			return false
		}
	}
	var input []map[string]json.RawMessage
	if json.Unmarshal(request["input"], &input) != nil || len(input) == 0 {
		return false
	}
	users := 0
	for _, item := range input {
		var kind, role string
		_ = json.Unmarshal(item["type"], &kind)
		_ = json.Unmarshal(item["role"], &role)
		if kind == "additional_tools" && role == "developer" {
			continue
		}
		if kind != "" && kind != "message" {
			return false
		}
		if role != "user" && role != "developer" && role != "system" {
			return false
		}
		if role == "user" {
			users++
		}
	}
	return users >= 1
}

func identity(r *http.Request) (threadEvidence, error) {
	var e threadEvidence
	for _, name := range []string{"Thread-Id", "Session-Id", "X-Codex-Turn-Metadata"} {
		if len(r.Header.Values(name)) > 1 {
			return e, errors.New("SESSION_ID_CONFLICT")
		}
	}
	id := r.Header.Get("Thread-Id")
	if !validID(id) {
		return e, errors.New("SESSION_ID_REQUIRED")
	}
	e.ThreadID = id
	e.SessionID = r.Header.Get("Session-Id")
	if e.SessionID != "" && !validID(e.SessionID) {
		return e, errors.New("SESSION_ID_CONFLICT")
	}
	if raw := r.Header.Get("X-Codex-Turn-Metadata"); raw != "" {
		metadata, err := parseThreadMetadata(raw)
		if err != nil {
			return e, errors.New("SESSION_METADATA_INVALID")
		}
		// The desktop's ephemeral warm fork keeps the parent in the Session-Id header,
		// while metadata consistently names the child. Accept only that observed shape;
		// Session-Id is never a binding key or a substitute for explicit parent evidence.
		parentSessionHeader := metadata.History == "warm_fork" &&
			metadata.ThreadID == id && metadata.SessionID == id &&
			validID(metadata.Parent) && metadata.Parent != id && e.SessionID == metadata.Parent
		if (metadata.ThreadID != "" && metadata.ThreadID != id) || (metadata.SessionID != "" && e.SessionID != "" && metadata.SessionID != e.SessionID && !parentSessionHeader) {
			return e, errors.New("SESSION_ID_CONFLICT")
		}
		if metadata.SessionID != "" {
			e.SessionID = metadata.SessionID
		}
		e.History, e.Parent = metadata.History, metadata.Parent
	}
	// The canonical metadata session must still agree with the child thread.
	if (e.SessionID != "" && e.SessionID != id) || (e.Parent != "" && (!validID(e.Parent) || e.Parent == id)) {
		return e, errors.New("SESSION_ID_CONFLICT")
	}
	return e, nil
}

// readInferenceBody 在 16 MiB 上限内读取完整正文，用于首次证据或模型覆盖。
func readInferenceBody(w http.ResponseWriter, r *http.Request, cfg api.Config) ([]byte, error) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(time.Duration(cfg.IdleTimeoutMS) * time.Millisecond))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEvidenceBody))
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}
	return data, nil
}

type clientBodyReader struct {
	io.ReadCloser
	controller *http.ResponseController // 只约束客户端正文的实际读取。
	timeout    time.Duration
}

func (body *clientBodyReader) Read(p []byte) (int, error) {
	_ = body.controller.SetReadDeadline(time.Now().Add(body.timeout))
	n, err := body.ReadCloser.Read(p)
	_ = body.controller.SetReadDeadline(time.Time{})
	return n, err
}

// client 按供应商隔离连接池；修改连接或响应头超时会重建全部池。
func (s *Server) client(providerID string, c api.Config) *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ConnectTimeoutMS != s.clientConfig.ConnectTimeoutMS || c.HeaderTimeoutMS != s.clientConfig.HeaderTimeoutMS {
		for _, old := range s.clients {
			old.CloseIdleConnections()
		}
		s.clients = make(map[string]*http.Client)
		s.clientConfig = c
	}
	if client, ok := s.clients[providerID]; ok {
		return client
	}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: time.Duration(c.ConnectTimeoutMS) * time.Millisecond, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    time.Duration(c.ConnectTimeoutMS) * time.Millisecond,
		ResponseHeaderTimeout:  time.Duration(c.HeaderTimeoutMS) * time.Millisecond,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           32, MaxIdleConnsPerHost: 16, MaxConnsPerHost: 128, IdleConnTimeout: 60 * time.Second,
		DisableCompression: true,
		ForceAttemptHTTP2:  true,
	}
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	s.clients[providerID] = client
	return client
}

// forward 解析会话固定供应商，按供应商隔离身份与模型覆盖后转发原始流。
func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := s.beginActivity(r)
	tracked := &activityWriter{ResponseWriter: w, started: started}
	cfg := s.state.config()
	if s.log != nil {
		tracked.diag = newRequestDiagnostic(s, requestID, started, r, cfg)
	}
	w = tracked
	defer func() {
		abnormal := recover()
		if r.Context().Err() != nil && tracked.code == "" {
			tracked.code = "REQUEST_CANCELED"
			tracked.stage = "client"
		}
		s.endActivity(requestID, tracked, abnormal != nil || tracked.failed || r.Context().Err() != nil)
		if abnormal != nil {
			panic(abnormal)
		}
	}()
	if r.URL.Path == "/responses" && r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		// 已验证 CLI 在 426 后自行改用 SSE；不将握手标识当作后续所有 WS 帧的身份。
		fail(w, http.StatusUpgradeRequired, "WEBSOCKET_UNSUPPORTED", "此构建仅支持 HTTP/SSE，客户端须自行选择兼容传输")
		return
	}
	if r.URL.Path == "/models" {
		if r.Method != http.MethodGet {
			fail(w, 405, "METHOD_NOT_ALLOWED", "模型列表仅支持 GET")
			return
		}
	} else if r.Method != http.MethodPost {
		fail(w, 405, "METHOD_NOT_ALLOWED", "推理入口仅支持 POST")
		return
	}
	if r.URL.IsAbs() || r.URL.Opaque != "" {
		fail(w, 400, "INVALID_PATH", "不接受绝对目标地址")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, 400, "INVALID_INPUT", "查询无效")
		return
	}
	for key := range q {
		if r.URL.Path != "/models" || key != "client_version" {
			fail(w, 400, "INVALID_INPUT", "该查询参数不受支持")
			return
		}
	}
	if s.active.Add(1) > int64(cfg.MaxConcurrent) {
		s.active.Add(-1)
		fail(w, 429, "CONCURRENCY_LIMIT", "转发并发已达上限")
		return
	}
	defer s.active.Add(-1)
	var providerID string
	var originalBody []byte
	if r.URL.Path == "/models" {
		// 全局模型目录不携带可靠会话证据，仅查询内置官方供应商，绝不据默认值发送到其他供应商。
		providerID = BuiltinOfficialID
	} else {
		e, eerr := identity(r)
		if eerr != nil {
			fail(w, 400, eerr.Error(), "缺少可靠且一致的 thread-id 与元数据")
			return
		}
		// 身份检查后有界读取原始请求，只检查首次证据，转发仍使用完全相同的字节。
		if r.URL.Path == "/responses" && e.History == "new" {
			s.state.mu.Lock()
			_, bound := s.state.disk.Bindings[e.ThreadID]
			s.state.mu.Unlock()
			if !bound {
				originalBody, err = readInferenceBody(w, r, cfg)
				if err != nil {
					fail(w, 413, "REQUEST_BODY_INVALID", "首次请求读取失败或超过 16 MiB")
					return
				}
				decoded, proven := decodeRequestEvidence(originalBody, r.Header.Values("Content-Encoding"))
				if !proven || !hasFreshInput(decoded) {
					e.History = "unproven"
				}
			}
		}
		if r.URL.Path != "/responses" && e.History == "new" {
			e.History = "unproven"
		}
		providerID, err = s.state.resolveEvidence(e)
		if err != nil {
			fail(w, 409, err.Error(), "会话未转发；请在页面确认供应商后由客户端重试")
			return
		}
		cfg = s.state.config()
	}
	s.activityProvider(requestID, providerID)
	tracked.provider = providerID
	provider, ok := s.state.providerRecord(providerID)
	if !ok {
		fail(w, 409, "SESSION_PROVIDER_MISSING", "会话绑定的供应商已不存在；请使用原数据目录或重新登记")
		return
	}
	if provider.BaseURL == "" {
		fail(w, 503, "UPSTREAM_NOT_CONFIGURED", "供应商尚未配置上游地址")
		return
	}
	// 模型覆盖需要完整、可解析的正文；未配置覆盖时仍原样流式转发。
	encodings := r.Header.Values("Content-Encoding")
	var body io.Reader
	contentLength := r.ContentLength
	if provider.Model != "" && r.Method == http.MethodPost {
		raw := originalBody
		if raw == nil {
			raw, err = readInferenceBody(w, r, cfg)
			if err != nil {
				fail(w, 413, "REQUEST_BODY_INVALID", "模型覆盖需要完整读取正文，读取失败或超过 16 MiB")
				return
			}
		}
		rewritten, rerr := applyModelOverride(raw, encodings, provider.Model)
		if rerr != nil {
			switch {
			case errors.Is(rerr, errModelEncodingUnsupported):
				fail(w, 400, "MODEL_OVERRIDE_ENCODING_UNSUPPORTED", "供应商启用了模型覆盖，但请求正文编码无法解码；请求未转发")
			case errors.Is(rerr, errModelBodyTooLarge):
				fail(w, 413, "REQUEST_BODY_INVALID", "模型覆盖后的正文超过 16 MiB；请求未转发")
			default:
				fail(w, 400, "MODEL_OVERRIDE_INVALID_BODY", "供应商启用了模型覆盖，但正文不是可解析的 JSON 对象；请求未转发")
			}
			return
		}
		body = bytes.NewReader(rewritten)
		contentLength = int64(len(rewritten))
		// 已按未压缩 JSON 重写，不能继续声明原压缩编码。
		encodings = nil
	} else if originalBody != nil {
		body = http.MaxBytesReader(w, io.NopCloser(bytes.NewReader(originalBody)), maxEvidenceBody)
		contentLength = int64(len(originalBody))
	} else {
		body = &clientBodyReader{ReadCloser: http.MaxBytesReader(w, r.Body, maxEvidenceBody), controller: http.NewResponseController(w), timeout: time.Duration(cfg.IdleTimeoutMS) * time.Millisecond}
	}
	target, err := url.Parse(strings.TrimRight(provider.BaseURL, "/") + r.URL.Path)
	if err != nil {
		fail(w, 500, "INVALID_CONFIG", "上游地址无效")
		return
	}
	target.RawQuery = r.URL.RawQuery
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	// 只用单次不可回放的 Body；不设置 GetBody，不使用 ReverseProxy 的重试机制。
	up, err := http.NewRequestWithContext(ctx, r.Method, target.String(), body)
	if err != nil {
		fail(w, 500, "REQUEST_FAILED", "构造转发失败")
		return
	}
	up.GetBody = nil
	up.ContentLength = contentLength
	if r.Method == http.MethodGet {
		up.Body = nil
		up.ContentLength = 0
	}
	up.Header.Set("User-Agent", "CodexRouter/0.1")
	copyProtocolHeaders(up.Header, r.Header, "Content-Type", "Accept", "Accept-Encoding")
	// 未重写时压缩正文保持原始字节，编码声明也须保持完整（包括多值）。
	for _, value := range encodings {
		up.Header.Add("Content-Encoding", value)
	}
	if provider.Kind == api.ProviderKindAPIKey {
		key, e := s.creds.Get(provider.ID)
		if e != nil || key == "" {
			fail(w, 503, "CREDENTIAL_UNAVAILABLE", "供应商凭据不可用")
			return
		}
		up.Header.Set("Authorization", "Bearer "+key)
	} else {
		copyProtocolHeaders(up.Header, r.Header, "User-Agent", "Authorization", "Chatgpt-Account-Id", "Openai-Organization", "Openai-Project", "Openai-Beta", "Originator", "Thread-Id", "Session-Id", "X-Codex-Turn-Metadata", "X-Codex-Turn-State", "X-Codex-Beta-Features", "Version", "X-Client-Request-Id", "X-Codex-Window-Id", "X-Openai-Internal-Codex-Responses-Lite")
	}
	controller := http.NewResponseController(w)
	if tracked.diag != nil {
		up = up.WithContext(httptrace.WithClientTrace(up.Context(), tracked.diag.trace()))
		if up.Body != nil {
			up.Body = &diagnosticBody{ReadCloser: up.Body, d: tracked.diag}
		}
	}
	resp, err := s.client(provider.ID, cfg).Do(up)
	if err != nil {
		if tracked.diag != nil {
			tracked.diag.failed(err)
			tracked.diag.emit("upstream_request_failed", diaglog.Fields{"network_error": diagnosticError(err), "provider_id": provider.ID})
		}
		code := networkCode(err)
		fail(w, 502, code, networkMessage(code))
		return
	}
	defer resp.Body.Close()
	if tracked.diag != nil {
		tracked.diag.headers(resp)
	}
	tracked.observation = newResponseObservation(resp, started, r.Method+" "+r.URL.Path)
	copyProtocolHeaders(w.Header(), resp.Header, "Content-Type", "Cache-Control", "Retry-After", "X-Request-Id", "Content-Encoding")
	if provider.Kind == api.ProviderKindOfficial {
		copyProtocolHeaders(w.Header(), resp.Header, "X-Codex-Turn-State")
	}
	w.Header().Set("Cache-Control", "no-store")
	_ = controller.SetWriteDeadline(time.Now().Add(time.Duration(cfg.WriteTimeoutMS) * time.Millisecond))
	w.WriteHeader(resp.StatusCode)
	if err = controller.Flush(); err != nil {
		tracked.failed = true
		tracked.code = "CLIENT_WRITE_FAILED"
		tracked.stage = "client"
		return
	}
	// 写截止只覆盖写入；等待上游期间残留截止会使正常HTTP收尾失败。
	_ = controller.SetWriteDeadline(time.Time{})
	buf := make([]byte, 32<<10)
	for {
		// 计时器只覆盖本次上游 Read；客户端背压有独立的写截止时间。
		timer := time.AfterFunc(time.Duration(cfg.IdleTimeoutMS)*time.Millisecond, cancel)
		n, re := resp.Body.Read(buf)
		arrived := time.Now()
		if tracked.diag != nil {
			tracked.diag.received(n, arrived)
		}
		stopped := timer.Stop()
		if n > 0 {
			_ = controller.SetWriteDeadline(time.Now().Add(time.Duration(cfg.WriteTimeoutMS) * time.Millisecond))
			written, writeErr := w.Write(buf[:n])
			if tracked.diag != nil {
				tracked.diag.written += int64(written)
			}
			if err = writeErr; err != nil {
				tracked.failed = true
				tracked.code = "CLIENT_WRITE_FAILED"
				tracked.stage = "client"
				return
			}
			if err = controller.Flush(); err != nil {
				tracked.failed = true
				tracked.code = "CLIENT_WRITE_FAILED"
				tracked.stage = "client"
				return
			}
			_ = controller.SetWriteDeadline(time.Time{})
			tracked.observation.feedAt(buf[:n], arrived)
		}
		if re != nil {
			if re == io.EOF && stopped {
				return
			}
			code := "STREAM_INTERRUPTED"
			if !stopped {
				code = "STREAM_IDLE_TIMEOUT"
			}
			code = streamFailureCode(r.Context(), s.ctx, code)
			tracked.code = code
			tracked.stage = "stream"
			if tracked.diag != nil {
				tracked.diag.emit("stream_read_failed", diaglog.Fields{"network_error": diagnosticError(re), "provider_id": provider.ID, "reason": code})
			}
			// 中断 HTTP 流而非伪造成功 EOF/完成事件，宿主会关闭 H1 或重置 H2。
			panic(http.ErrAbortHandler)
		}
	}
}

// streamFailureCode 将可确认的客户端取消及路由器关闭与上游断流区分。
func streamFailureCode(request, server context.Context, fallback string) string {
	if request.Err() != nil {
		return "REQUEST_CANCELED"
	}
	if server.Err() != nil {
		return "ROUTER_SHUTDOWN"
	}
	return fallback
}

func networkCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "REQUEST_CANCELED"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "UPSTREAM_DNS_FAILED"
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return "UPSTREAM_TLS_FAILED"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "UPSTREAM_TIMEOUT"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return "UPSTREAM_CONNECTION_CLOSED"
	}
	return "UPSTREAM_UNREACHABLE"
}

// networkMessage 只输出固定分类，不泄露上游地址、凭据或原始网络错误。
func networkMessage(code string) string {
	message := "无法连接上游"
	switch code {
	case "REQUEST_CANCELED":
		message = "请求已取消"
	case "UPSTREAM_DNS_FAILED":
		message = "上游域名解析失败"
	case "UPSTREAM_TLS_FAILED":
		message = "上游 TLS 证书验证失败"
	case "UPSTREAM_TIMEOUT":
		message = "连接或等待上游响应超时，请检查线路和响应头等待设置"
	case "UPSTREAM_CONNECTION_CLOSED":
		message = "上游在返回响应头前断开连接"
	}
	return code + ": " + message + "；未重放或切换供应商"
}
