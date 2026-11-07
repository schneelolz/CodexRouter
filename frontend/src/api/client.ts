import type {
  AuthSession, Binding, Capabilities, Config, ConfigInput, CredentialStatus,
  Diagnostics, ErrorResponse, Provider, ProviderCreateInput, ProviderCredentials,
  ProviderUpdateInput, Providers, RegisterInput, Sessions, Status, Clients, ClientAction, ProviderSaveInput, ModelQueryInput, Models, DailyStatus, DailyPreview, DailyAction, DailyApply, Activity, RequestPage, SessionPage, HistoryPolicy, ClearHistoryResult,
} from './types.gen'

/** 表示可供界面判断的管理接口错误。 */
export class ApiError extends Error {
  constructor(public readonly code: string, message: string, public readonly status = 0) {
    super(message)
  }
}

/** 说明 fetch 传输函数，供 mock 和独立测试注入。 */
export type FetchTransport = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>

/** 校验管理接口的主版本与最低次版本。 */
export function assertCompatible(capabilities: Capabilities) {
  const [major, minor] = capabilities.api_version.split('.').map(x => Number.parseInt(x, 10))
  if (major !== 1 || !Number.isFinite(minor) || minor < 7) {
    throw new ApiError('API_VERSION_INCOMPATIBLE', `管理接口版本 ${capabilities.api_version} 不兼容，请使用配套前端。`)
  }
}

/** 立即移除 URL 中的引导令牌，只返回给当前页面内存。 */
export function consumeBootstrap(location: Pick<Location, 'hash' | 'pathname' | 'search'>, history: Pick<History, 'replaceState'>) {
  const token = new URLSearchParams(location.hash.slice(1)).get('token')
  if (token !== null) history.replaceState(null, '', location.pathname + location.search)
  return token
}

/** 管理 API 客户端；CSRF 令牌只存在于该实例内存。 */
export class ApiClient {
  private csrf = ''
  constructor(private readonly transport: FetchTransport = (input, init) => globalThis.fetch(input, init)) {}

  private async request<T>(path: string, method = 'GET', body?: unknown, signal?: AbortSignal): Promise<T> {
    const controller = new AbortController()
    const timeout = setTimeout(() => controller.abort(), 10000)
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (method !== 'GET' && this.csrf) headers['X-CSRF-Token'] = this.csrf
    try {
      const response = await this.transport(`/api/v1/${path}`, {
        method, headers, body: body === undefined ? undefined : JSON.stringify(body),
        credentials: 'same-origin', cache: 'no-store',
        signal: signal ? AbortSignal.any([signal, controller.signal]) : controller.signal,
      })
      if (!response.ok) {
        let error: ErrorResponse | undefined
        try { error = await response.json() as ErrorResponse } catch { /* 非 JSON 错误保持通用说明。 */ }
        throw new ApiError(error?.error?.code ?? 'HTTP_ERROR', error?.error?.message ?? `请求失败（${response.status}），请重试。`, response.status)
      }
      return await response.json() as T
    } catch (error) {
      if (error instanceof ApiError) throw error
      if (signal?.aborted) throw error
      throw new ApiError('BACKEND_UNREACHABLE', '无法连接本地服务。请确认 CodexRouter 正在运行，再刷新重试。')
    } finally { clearTimeout(timeout) }
  }

  async authenticate(token: string | null, signal?: AbortSignal) {
    const session = token
      ? await this.request<AuthSession>('auth/exchange', 'POST', { token }, signal)
      : await this.request<AuthSession>('auth/session', 'GET', undefined, signal)
    signal?.throwIfAborted()
    this.csrf = session.csrf_token
  }

  daily(signal?: AbortSignal) { return this.request<DailyStatus>('daily', 'GET', undefined, signal) }
  dailyPreview(input: DailyAction, signal?: AbortSignal) { return this.request<DailyPreview>('daily/preview', 'POST', input, signal) }
  dailyApply(input: DailyApply, signal?: AbortSignal) { return this.request<DailyStatus>('daily/apply', 'POST', input, signal) }
  activity(signal?: AbortSignal) { return this.request<Activity>('activity', 'GET', undefined, signal) }
  async renew(signal?: AbortSignal) {
    try { return await this.request<AuthSession>('auth/renew', 'POST', undefined, signal) }
    catch (error) {
      // 仅恢复共享 Cookie 对应的 CSRF；业务写操作不会因此重放。
      if (!(error instanceof ApiError) || error.status !== 403 || error.code !== 'CSRF_REQUIRED') throw error
      await this.authenticate(null, signal)
      return this.request<AuthSession>('auth/renew', 'POST', undefined, signal)
    }
  }
  requestPage(limit: number, page: number, snapshot: string, filter: string, signal?: AbortSignal) { return this.request<RequestPage>(`requests?${new URLSearchParams({ limit: String(limit), page: String(page), snapshot, filter })}`, 'GET', undefined, signal) }
  requests(limit: number, cursor: string, filter: string, signal?: AbortSignal) { return this.request<RequestPage>(`requests?${new URLSearchParams({ limit: String(limit), cursor, filter })}`, 'GET', undefined, signal) }
  sessionPage(kind: string, limit = 20, cursor = '', signal?: AbortSignal) { return this.request<SessionPage>(`sessions/page?${new URLSearchParams({ kind, limit: String(limit), cursor })}`, 'GET', undefined, signal) }
  historyPolicy(signal?: AbortSignal) { return this.request<HistoryPolicy>('history/policy', 'GET', undefined, signal) }
  saveHistoryPolicy(days: number, signal?: AbortSignal) { return this.request<HistoryPolicy>('history/policy', 'PUT', { days }, signal) }
  clearHistory(signal?: AbortSignal) { return this.request<ClearHistoryResult>('history/clear', 'POST', { confirm: true }, signal) }
  clients(signal?: AbortSignal) { return this.request<Clients>('clients', 'GET', undefined, signal) }
  clientAction(input: ClientAction, signal?: AbortSignal) { return this.request<Clients>('clients/action', 'POST', input, signal) }
  capabilities(signal?: AbortSignal) { return this.request<Capabilities>('capabilities', 'GET', undefined, signal) }
  config(signal?: AbortSignal) { return this.request<Config>('config', 'GET', undefined, signal) }
  saveConfig(input: ConfigInput, signal?: AbortSignal) { return this.request<Config>('config', 'PUT', input, signal) }
  status(signal?: AbortSignal) { return this.request<Status>('status', 'GET', undefined, signal) }
  providers(signal?: AbortSignal) { return this.request<Providers>('providers', 'GET', undefined, signal) }
  saveRelay(input: ProviderSaveInput, signal?: AbortSignal) { return this.request<Provider>('providers/save', 'POST', input, signal) }
  queryModels(input: ModelQueryInput, signal?: AbortSignal) { return this.request<Models>('providers/models', 'POST', input, signal) }
  createProvider(input: ProviderCreateInput, signal?: AbortSignal) { return this.request<Provider>('providers', 'POST', input, signal) }
  updateProvider(id: string, input: ProviderUpdateInput, signal?: AbortSignal) { return this.request<Provider>(`providers/${encodeURIComponent(id)}`, 'PUT', input, signal) }
  deleteProvider(id: string, signal?: AbortSignal) { return this.request<Providers>(`providers/${encodeURIComponent(id)}`, 'DELETE', undefined, signal) }
  credentials(signal?: AbortSignal) { return this.request<ProviderCredentials>('credentials', 'GET', undefined, signal) }
  saveKey(providerId: string, key: string, signal?: AbortSignal) { return this.request<CredentialStatus>(`credentials/${encodeURIComponent(providerId)}`, 'PUT', { key }, signal) }
  deleteKey(providerId: string, signal?: AbortSignal) { return this.request<CredentialStatus>(`credentials/${encodeURIComponent(providerId)}`, 'DELETE', undefined, signal) }
  sessions(signal?: AbortSignal) { return this.request<Sessions>('sessions', 'GET', undefined, signal) }
  register(input: RegisterInput, signal?: AbortSignal) { return this.request<Binding>('sessions/register', 'POST', input, signal) }
  diagnostics(signal?: AbortSignal) { return this.request<Diagnostics>('diagnostics', 'GET', undefined, signal) }
}
