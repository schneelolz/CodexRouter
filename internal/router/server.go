package router

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codexrouter/internal/api"
	"codexrouter/internal/clients"
	"codexrouter/internal/daily"
	"codexrouter/internal/diaglog"
	"codexrouter/internal/history"
	"codexrouter/internal/platform"
)

// Options 定义 API、凭据和可替换静态目录的边界。
type Options struct {
	// Log 提供可选的有界诊断日志。
	Log *diaglog.Logger
	// HistoryPath 指定独立历史库；留空仅供内存夹具。
	HistoryPath string
	// Store 提供配置与绑定的原子持久化。
	Store Persistence
	// Credentials 提供平台安全凭据存储。
	Credentials platform.Credentials
	// Authority 指定精确的 loopback Host 和端口。
	Authority string
	// DevOrigin 显式允许固定 Vite 开发源。
	DevOrigin string
	// UIDir 指定独立构建的静态文件目录。
	UIDir string
	// Clients 只管理专用客户端；未配置时管理入口显式报告不可用。
	Clients *clients.Manager
	// Daily 仅管理日常配置连接字段，不管理日常进程。
	Daily *daily.Manager
}

// Server 在同一 loopback 地址提供互相隔离的管理与推理入口。
type Server struct {
	log             *diaglog.Logger               // 进程拥有的异步日志器，服务不负责关闭。
	history         *history.Store                // 独立展示历史，不是路由绑定权威。
	historyDegraded atomic.Bool                   // 本轮出现过记录缺口时持续提示。
	activityIDs     map[int]int                   // 当前进程请求与数据库主键的对应。
	activityStarted map[int]time.Time             // 使用单调时钟测量耗时。
	dailyManager    *daily.Manager                // 日常接入与恢复。
	activities      []api.RequestActivity         // 最近实际收到的请求，不含正文。
	received        int                           // 本进程累计入口请求数。
	clientManager   *clients.Manager              // 本实例拥有的专用测试客户端。
	state           *state                        // 配置、供应商与绑定。
	creds           platform.Credentials          // 平台安全凭据。
	authority       string                        // 精确监听地址。
	origins         map[string]bool               // 明确允许的同源或开发源。
	ui              http.Handler                  // 独立静态目录。
	uiRoot          *os.Root                      // 静态目录句柄，退出时释放。
	modelQueries    atomic.Int64                  // 独立限制模型查询并发。
	active          atomic.Int64                  // 转发中的请求数。
	mu              sync.Mutex                    // 诊断、认证和连接池保护。
	bootstrap       string                        // 单次引导值摘要。
	bootstrapExpiry time.Time                     // 引导有效期。
	session         string                        // 单浏览器管理会话摘要。
	csrf            string                        // 当前会话防伪值。
	sessionExpiry   time.Time                     // 会话期限。
	diagnostics     []api.Diagnostic              // 有界脱敏事件。
	upstreams       map[string]api.ProviderStatus // 按供应商记录最近结果。
	clients         map[string]*http.Client       // 按供应商分离连接池。
	clientConfig    api.Config                    // 池使用的超时配置。
	ctx             context.Context               // 服务退出取消信号。
	cancel          context.CancelFunc            // 中断剩余流。
}

// New 加载持久状态；存储或凭据失败时拒绝启动。
func New(o Options) (*Server, error) {
	if o.Credentials == nil || o.Store == nil {
		return nil, errors.New("缺少存储或凭据后端")
	}
	st, err := loadState(o.Store)
	if err != nil {
		return nil, err
	}
	st.creds = o.Credentials
	if _, err = o.Credentials.Get("control"); errors.Is(err, platform.ErrNotFound) {
		err = o.Credentials.Set("control", randomToken())
	}
	if err != nil {
		return nil, errors.New("本地控制凭据不可用")
	}
	s := &Server{log: o.Log, state: st, creds: o.Credentials, authority: o.Authority, origins: map[string]bool{"http://" + o.Authority: true}, clients: make(map[string]*http.Client), diagnostics: []api.Diagnostic{}, upstreams: make(map[string]api.ProviderStatus)}
	s.clientManager = o.Clients
	s.dailyManager = o.Daily
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if o.DevOrigin != "" {
		if o.DevOrigin != "http://127.0.0.1:18789" {
			s.cancel()
			return nil, errors.New("开发源仅支持 http://127.0.0.1:18789")
		}
		s.origins[o.DevOrigin] = true
	}
	if o.UIDir != "" {
		s.ui, s.uiRoot, err = openStaticHandler(o.UIDir)
		if err != nil {
			s.cancel()
			return nil, err
		}
	}
	path := o.HistoryPath
	if path == "" {
		path = ":memory:"
	}
	s.history, err = history.Open(path)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.activityIDs = make(map[int]int)
	s.activityStarted = make(map[int]time.Time)
	st.history = s.history
	st.historyFailed = func() { s.historyDegraded.Store(true) }
	if err = st.initializeHistory(); err != nil {
		s.Close()
		return nil, err
	}
	if _, err = s.history.Prune(time.Now().UTC()); err != nil {
		s.Close()
		return nil, errors.New("历史保留策略执行失败")
	}
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case now := <-ticker.C:
				if _, err := s.history.Prune(now); err != nil {
					s.historyDegraded.Store(true)
				}
			}
		}
	}()
	return s, nil
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("系统随机源不可用")
	}
	return hex.EncodeToString(b[:])
}
func digest(v string) string { d := sha256.Sum256([]byte(v)); return hex.EncodeToString(d[:]) }
func equal(a, b string) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Bootstrap 创建五分钟有效的单次浏览器引导值，不写入日志。
func (s *Server) Bootstrap() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := randomToken()
	s.bootstrap = digest(v)
	s.bootstrapExpiry = time.Now().Add(5 * time.Minute)
	s.authEvent("bootstrap_issued", "local_open")
	return v
}

// Close 中断转发并回收空闲连接，供限时排空失败后调用。
func (s *Server) Close() {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		c.CloseIdleConnections()
	}
	if s.history != nil {
		_ = s.history.Close()
	}
	if s.uiRoot != nil {
		_ = s.uiRoot.Close()
		s.uiRoot = nil
	}
}

func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	if tracked, ok := w.(*activityWriter); ok {
		tracked.code = code
		tracked.stage = "routing"
		if status == 502 {
			tracked.stage = "upstream"
		}
	}
	respond(w, status, api.ErrorResponse{Error: api.ErrorDetail{Code: code, Message: message}})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "INVALID_INPUT", "请使用 application/json")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "INVALID_INPUT", "JSON 格式、字段或大小无效")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		fail(w, 400, "INVALID_INPUT", "仅允许一个 JSON 对象")
		return false
	}
	return true
}

// ServeHTTP 不开放 CORS，不信任 Forwarded 或 X-Forwarded-*。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'")
	if r.Host != s.authority {
		s.authEvent("request_rejected", "host_mismatch")
		fail(w, 403, "INVALID_HOST", "Host 不在允许列表")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !s.origins[origin] {
		s.authEvent("request_rejected", "origin_mismatch")
		fail(w, 403, "INVALID_ORIGIN", "拒绝外站请求")
		return
	}
	if r.URL.RawQuery != "" && strings.HasPrefix(r.URL.Path, "/api/") && !(r.Method == http.MethodGet && (r.URL.Path == "/api/v1/requests" || r.URL.Path == "/api/v1/sessions/page")) {
		fail(w, 400, "INVALID_INPUT", "管理接口不接受查询参数")
		return
	}
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		respond(w, 200, map[string]string{"status": "ok"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.manage(w, r)
		return
	}
	if r.URL.Path == "/responses" || r.URL.Path == "/responses/compact" || r.URL.Path == "/models" {
		s.forward(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		fail(w, 404, "INVALID_PATH", "连接基址应为本机根地址，不附加 /v1")
		return
	}
	if s.ui != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		s.ui.ServeHTTP(w, r)
		return
	}
	fail(w, 404, "NOT_FOUND", "未找到接口或未启用前端目录")
}

func (s *Server) authenticated(r *http.Request) (string, bool) {
	c, e := r.Cookie("codexrouter_session")
	if e != nil {
		s.authEvent("auth_rejected", "cookie_missing")
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !time.Now().Before(s.sessionExpiry) {
		s.authEvent("auth_rejected", "session_expired")
		return "", false
	}
	if !equal(s.session, digest(c.Value)) {
		s.authEvent("auth_rejected", "cookie_mismatch")
		return "", false
	}
	return s.csrf, true
}

func (s *Server) manage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/auth/exchange" && r.Method == http.MethodPost {
		if !s.origins[r.Header.Get("Origin")] {
			fail(w, 403, "INVALID_ORIGIN", "引导认证需要明确的同源 Origin")
			return
		}
		var in api.AuthInput
		if !decode(w, r, &in) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if time.Now().After(s.bootstrapExpiry) || !equal(s.bootstrap, digest(in.Token)) {
			reason := "bootstrap_invalid_or_consumed"
			if time.Now().After(s.bootstrapExpiry) {
				reason = "bootstrap_expired"
			}
			s.authEvent("auth_rejected", reason)
			fail(w, 401, "AUTH_REQUIRED", "引导链接无效或已使用，请从程序重新打开页面")
			return
		}
		if s.session != "" {
			s.authEvent("session_replaced", "new_bootstrap")
		}
		s.bootstrap = ""
		token := randomToken()
		s.session = digest(token)
		s.csrf = randomToken()
		s.sessionExpiry = time.Now().Add(8 * time.Hour)
		s.authEvent("bootstrap_exchanged", "ok")
		http.SetCookie(w, &http.Cookie{Name: "codexrouter_session", Value: token, Path: "/api/v1/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
		respond(w, 200, api.AuthSession{CSRFToken: s.csrf})
		return
	}
	csrf, ok := s.authenticated(r)
	if !ok {
		fail(w, 401, "AUTH_REQUIRED", "管理认证已失效，请从程序重新打开页面")
		return
	}
	if r.Method != http.MethodGet && (!s.origins[r.Header.Get("Origin")] || !equal(csrf, r.Header.Get("X-CSRF-Token"))) {
		reason := "csrf_mismatch"
		if !s.origins[r.Header.Get("Origin")] {
			reason = "origin_mismatch"
		}
		s.authEvent("auth_rejected", reason)
		fail(w, 403, "CSRF_REQUIRED", "写操作需要同源 Origin 与 CSRF 凭据")
		return
	}
	switch {
	case r.URL.Path == "/api/v1/auth/renew":
		if r.Method != http.MethodPost {
			fail(w, 405, "METHOD_NOT_ALLOWED", "认证续期仅支持POST")
			return
		}
		s.renewSession(w, r)
		return
	case r.URL.Path == "/api/v1/daily" || r.URL.Path == "/api/v1/daily/preview" || r.URL.Path == "/api/v1/daily/apply":
		s.manageDaily(w, r)
		return
	case r.URL.Path == "/api/v1/requests" || r.URL.Path == "/api/v1/sessions/page" || strings.HasPrefix(r.URL.Path, "/api/v1/history/"):
		s.manageHistory(w, r)
		return
	case r.URL.Path == "/api/v1/activity":
		if r.Method == http.MethodGet {
			s.mu.Lock()
			out := api.Activity{Received: s.received, Items: append([]api.RequestActivity{}, s.activities...), Degraded: s.historyDegraded.Load()}
			s.mu.Unlock()
			respond(w, 200, out)
			return
		}
	case r.URL.Path == "/api/v1/clients":
		if r.Method == http.MethodGet {
			if s.clientManager == nil {
				respond(w, 200, clients.Status{Items: []clients.Client{}})
			} else {
				respond(w, 200, s.clientManager.Status())
			}
			return
		}
	case r.URL.Path == "/api/v1/clients/action":
		if r.Method == http.MethodPost {
			var in clients.Input
			if !decode(w, r, &in) {
				return
			}
			if s.clientManager == nil {
				fail(w, 503, "CLIENT_UNAVAILABLE", "当前实例未启用专用客户端管理")
				return
			}
			out, err := s.clientManager.Action(in)
			if err != nil {
				code := err.Error()
				message := "专用客户端操作失败，未修改日常配置"
				status := 409
				switch code {
				case "CLIENT_START_FAILED":
					message = "测试窗口创建失败，请检查客户端安装及 Windows 控制台权限；未修改日常配置"
				case "CLIENT_RUNNING":
					message = "请先关闭或停止该专用客户端，再切换模式或重新启动"
				case "PROFILE_CONFLICT":
					message = "专用配置被外部修改，已停止操作以避免覆盖"
				case "CLIENT_NOT_INSTALLED":
					message = "未检测到可安全启动的客户端"
				case "INVALID_CLIENT_ACTION":
					status = 400
					message = "客户端动作不受支持"
				default:
					code = "CLIENT_ACTION_FAILED"
				}
				fail(w, status, code, message)
				return
			}
			respond(w, 200, out)
			return
		}
	case r.URL.Path == "/api/v1/auth/session":
		if r.Method == http.MethodGet {
			respond(w, 200, api.AuthSession{CSRFToken: csrf})
			return
		}
	case r.URL.Path == "/api/v1/capabilities":
		if r.Method == http.MethodGet {
			respond(w, 200, api.Capabilities{APIVersion: "1.8", ProtocolStatus: "experimental", Transports: []string{"http-sse"}})
			return
		}
	case r.URL.Path == "/api/v1/status":
		if r.Method == http.MethodGet {
			respond(w, 200, api.Status{Local: "running", ActiveRequests: int(s.active.Load()), Upstreams: s.upstreamStates()})
			return
		}
	case r.URL.Path == "/api/v1/config":
		s.manageConfig(w, r)
		return
	case r.URL.Path == "/api/v1/providers":
		s.manageProviders(w, r)
		return
	case r.URL.Path == "/api/v1/providers/save":
		s.manageRelaySave(w, r)
		return
	case r.URL.Path == "/api/v1/providers/models":
		s.manageRelayModels(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/providers/"):
		s.manageProvider(w, r)
		return
	case r.URL.Path == "/api/v1/credentials":
		if r.Method == http.MethodGet {
			items, err := s.state.credentialStatuses()
			if err != nil {
				fail(w, 503, "CREDENTIAL_UNAVAILABLE", "系统凭据存储不可用")
				return
			}
			respond(w, 200, items)
			return
		}
	case strings.HasPrefix(r.URL.Path, "/api/v1/credentials/"):
		s.manageCredential(w, r)
		return
	case r.URL.Path == "/api/v1/sessions":
		if r.Method == http.MethodGet {
			respond(w, 200, s.state.sessions())
			return
		}
	case r.URL.Path == "/api/v1/sessions/register":
		if r.Method == http.MethodPost {
			var in api.RegisterInput
			if !decode(w, r, &in) {
				return
			}
			b, e := s.state.register(in)
			if e != nil {
				fail(w, 409, e.Error(), "首次登记失败；请检查待登记项、供应商配置及存储")
				return
			}
			respond(w, 200, b)
			return
		}
	case r.URL.Path == "/api/v1/diagnostics":
		if r.Method == http.MethodGet {
			s.mu.Lock()
			items := append([]api.Diagnostic{}, s.diagnostics...)
			s.mu.Unlock()
			stats := s.log.Snapshot()
			if stats.Dropped > 0 {
				items = append(items, api.Diagnostic{Time: time.Now().UTC().Format(time.RFC3339), Code: "LOG_DROPPED", ProviderID: "local"})
			}
			if stats.WriteFailures > 0 {
				items = append(items, api.Diagnostic{Time: time.Now().UTC().Format(time.RFC3339), Code: "LOG_WRITE_FAILED", ProviderID: "local"})
			}
			respond(w, 200, api.Diagnostics{Items: items})
			return
		}
	default:
		fail(w, 404, "NOT_FOUND", "未知管理接口")
		return
	}
	fail(w, 405, "METHOD_NOT_ALLOWED", "该接口不支持此方法")
}

// manageConfig 只处理默认供应商、超时与并发；供应商本身有独立接口。
func (s *Server) manageConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		respond(w, 200, s.state.config())
	case http.MethodPut:
		var in api.ConfigInput
		if !decode(w, r, &in) {
			return
		}
		next := api.Config{Version: stateVersion, DefaultProviderID: in.DefaultProviderID, ConnectTimeoutMS: in.ConnectTimeoutMS, HeaderTimeoutMS: in.HeaderTimeoutMS, IdleTimeoutMS: in.IdleTimeoutMS, WriteTimeoutMS: in.WriteTimeoutMS, MaxConcurrent: in.MaxConcurrent}
		if err := s.state.saveConfig(next); err != nil {
			fail(w, 409, "CONFIG_SAVE_FAILED", safeConfigError(err))
			return
		}
		respond(w, 200, s.state.config())
	default:
		fail(w, 405, "METHOD_NOT_ALLOWED", "配置仅支持读取与保存")
	}
}

func (s *Server) manageProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.state.providers()
		if err != nil {
			fail(w, 503, "CREDENTIAL_UNAVAILABLE", "系统凭据存储不可用")
			return
		}
		respond(w, 200, items)
	case http.MethodPost:
		var in api.ProviderCreateInput
		if !decode(w, r, &in) {
			return
		}
		created, err := s.state.addProvider(in)
		if err != nil {
			status, code, message := providerError(err)
			fail(w, status, code, message)
			return
		}
		respond(w, 200, created)
	default:
		fail(w, 405, "METHOD_NOT_ALLOWED", "供应商集合仅支持读取与新建")
	}
}

func (s *Server) manageProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := subresourceID(r.URL.Path, "/api/v1/providers/")
	if !ok {
		fail(w, 404, "PROVIDER_NOT_FOUND", "供应商标识无效或不存在")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var in api.ProviderUpdateInput
		if !decode(w, r, &in) {
			return
		}
		updated, err := s.state.updateProvider(id, in)
		if err != nil {
			status, code, message := providerError(err)
			fail(w, status, code, message)
			return
		}
		respond(w, 200, updated)
	case http.MethodDelete:
		items, err := s.state.deleteProvider(id)
		if err != nil {
			status, code, message := providerError(err)
			fail(w, status, code, message)
			return
		}
		respond(w, 200, items)
	default:
		fail(w, 405, "METHOD_NOT_ALLOWED", "单个供应商仅支持更新与删除")
	}
}

// manageCredential 以供应商标识作为系统凭据名称，只接受写入和删除。
func (s *Server) manageCredential(w http.ResponseWriter, r *http.Request) {
	id, ok := subresourceID(r.URL.Path, "/api/v1/credentials/")
	if !ok {
		fail(w, 404, "PROVIDER_NOT_FOUND", "供应商标识无效或不存在")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var in api.CredentialInput
		if !decode(w, r, &in) {
			return
		}
		if len(in.Key) < 1 || len(in.Key) > 2560 || strings.ContainsAny(in.Key, "\r\n\x00") {
			fail(w, 400, "INVALID_INPUT", "密钥须为 1 至 2560 字节且不含控制换行")
			return
		}
		status, err := s.state.setProviderKey(id, in.Key)
		if err != nil {
			code, message := credentialError(err)
			fail(w, code.status, code.code, message)
			return
		}
		respond(w, 200, status)
	case http.MethodDelete:
		status, err := s.state.deleteProviderKey(id)
		if err != nil {
			code, message := credentialError(err)
			fail(w, code.status, code.code, message)
			return
		}
		respond(w, 200, status)
	default:
		fail(w, 405, "METHOD_NOT_ALLOWED", "凭据仅支持写入与删除")
	}
}

// subresourceID 只接受单段供应商标识，拒绝额外路径和查询。
func subresourceID(path, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(path, prefix)
	if !ok || rest == "" || strings.Contains(rest, "/") || !validProviderID(rest) {
		return "", false
	}
	return rest, true
}

// providerError 把状态层稳定错误码映射为 HTTP 状态与用户可读说明。
func providerError(err error) (int, string, string) {
	switch err.Error() {
	case "INVALID_INPUT":
		return 400, "INVALID_INPUT", "供应商名称、基址、模型或认证方式无效"
	case "MODEL_QUERY_KEY_REQUIRED":
		return 409, "MODEL_QUERY_KEY_REQUIRED", "地址已改变，请通过统一保存重新输入该地址的 API Key"
	case "PROVIDER_NOT_FOUND":
		return 404, "PROVIDER_NOT_FOUND", "供应商不存在，可能已被删除"
	case "PROVIDER_LIMIT":
		return 409, "PROVIDER_LIMIT", "供应商数量已达上限 8 个"
	case "PROVIDER_LOCKED":
		return 409, "PROVIDER_LOCKED", "该供应商已有固定会话，上游基址不可修改"
	case "PROVIDER_BUILTIN":
		return 409, "PROVIDER_BUILTIN", "内置官方供应商不能删除"
	case "PROVIDER_BOUND":
		return 409, "PROVIDER_BOUND", "该供应商仍有固定会话，不能删除"
	case "PROVIDER_DEFAULT":
		return 409, "PROVIDER_DEFAULT", "请先把新会话默认供应商切换到其他线路"
	case "CREDENTIAL_UNAVAILABLE":
		return 503, "CREDENTIAL_UNAVAILABLE", "系统凭据存储不可用，本次变更未保存"
	case "CREDENTIAL_RESTORE_FAILED":
		return 500, "CREDENTIAL_RESTORE_FAILED", "供应商未删除，但系统凭据无法恢复；请重新录入该供应商密钥"
	case "PROVIDER_ID_FAILED":
		return 500, "PROVIDER_ID_FAILED", "无法生成供应商标识，请重试"
	default:
		return 500, "STORAGE_WRITE_FAILED", "供应商未保存；原配置仍有效"
	}
}

// credentialError 区分供应商不存在、官方供应商和凭据后端失败。
func credentialError(err error) (struct {
	status int
	code   string
}, string) {
	var out struct {
		status int
		code   string
	}
	switch err.Error() {
	case "PROVIDER_NOT_FOUND":
		out.status, out.code = 404, "PROVIDER_NOT_FOUND"
		return out, "供应商不存在，可能已被删除"
	case "PROVIDER_NOT_API_KEY":
		out.status, out.code = 409, "PROVIDER_NOT_API_KEY"
		return out, "官方供应商转发客户端自身身份，不保存 API 密钥"
	default:
		out.status, out.code = 503, "CREDENTIAL_UNAVAILABLE"
		return out, "系统凭据存储不可用"
	}
}

func safeConfigError(err error) string {
	for _, fragment := range []string{"上游", "超时", "并发", "配置版本", "默认供应商", "供应商"} {
		if strings.Contains(err.Error(), fragment) {
			return err.Error()
		}
	}
	return "配置持久化失败，原配置仍有效"
}

// upstreamStates 为当前每个供应商生成最近结果，未请求过时保持未验证。
func (s *Server) upstreamStates() []api.ProviderStatus {
	s.mu.Lock()
	recorded := make(map[string]api.ProviderStatus, len(s.upstreams))
	for id, st := range s.upstreams {
		recorded[id] = st
	}
	s.mu.Unlock()
	ids := s.state.providerIDs()
	items := make([]api.ProviderStatus, 0, len(ids))
	for _, id := range ids {
		st, ok := recorded[id]
		if !ok {
			st = api.ProviderStatus{ProviderID: id, Result: "unverified"}
		}
		st.ProviderID = id
		items = append(items, st)
	}
	return items
}

func (s *Server) record(providerID, code string, success bool) {
	result := "failure"
	if success {
		result = "success"
	}
	s.recordStatus(providerID, code, result)
}

// recordStatus 允许未能识别结果的请求保持未验证，不冒充成功或上游故障。
func (s *Server) recordStatus(providerID, code, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	s.upstreams[providerID] = api.ProviderStatus{ProviderID: providerID, Result: result, CheckedAt: now, Code: code}
	s.diagnostics = append(s.diagnostics, api.Diagnostic{Time: now, Code: code, ProviderID: providerID})
	if len(s.diagnostics) > 200 {
		s.diagnostics = s.diagnostics[len(s.diagnostics)-200:]
	}
}

// HTTPServer 返回无短总写超时的 HTTP 宿主，流内单独设置截止时间。
func HTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
}

func openStaticHandler(dir string) (http.Handler, *os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	if f, e := root.Open("index.html"); e != nil {
		root.Close()
		return nil, nil, errors.New("前端目录缺少 index.html")
	} else {
		f.Close()
	}
	// os.Root 阻止绝对路径、父路径与符号链接逃逸，不使用 SPA 兜底吞掉 API 404。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if strings.ContainsAny(name, "\\\x00") || strings.Contains(name, "..") || strings.HasPrefix(name, ".") {
			http.NotFound(w, r)
			return
		}
		f, e := root.Open(name)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name, info.ModTime(), f)
	}), root, nil
}
