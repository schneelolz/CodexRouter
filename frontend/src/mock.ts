import type { FetchTransport } from './api/client'
import type { Config, Provider, ProviderCreateInput, ProviderUpdateInput, RegisterInput, Sessions, Diagnostic, ProviderSaveInput, DailyStatus } from './api/types.gen'

/** 创建仅用于开发及测试的合成管理 API。 */
export function createMockApi(): FetchTransport {
  let config: Config = {
    version: 2, default_provider_id: 'official',
    connect_timeout_ms: 10000, header_timeout_ms: 30000, idle_timeout_ms: 120000, write_timeout_ms: 30000, max_concurrent: 16,
  }
  let providers: Provider[] = [
    { id: 'official', name: '官方线路', kind: 'official', base_url: 'https://chatgpt.com/backend-api/codex', model: '', key_configured: false, upstream_locked: true },
    { id: 'company', name: '公司线路', kind: 'api_key', base_url: '', model: '', key_configured: false, upstream_locked: false },
  ]
  let daily: DailyStatus = { state: 'direct', config_path: 'C:/Synthetic/Codex/config.toml', base_url: 'http://127.0.0.1:18788', has_backup: false, message: '未接入日常配置' }
  let dailyAction = ''
  let dailyToken = ''
  const keys = new Set<string>()
  let sequence = 0
  let retentionDays = 0
  const sessions: Sessions = {
    bindings: [{ id: 'demo-official-session', provider_id: 'official', created_at: new Date().toISOString() }],
    pending: [{ id: 'demo-pending-session', first_seen: new Date().toISOString(), reason: 'unknown_origin' }],
  }
  const diagnostics: Diagnostic[] = [{ time: new Date().toISOString(), code: 'SESSION_PENDING', provider_id: 'official' }]
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
  const parse = <T>(init?: RequestInit): T => {
    try { return JSON.parse(String(init?.body)) as T } catch { return {} as T }
  }
  return async (input, init) => {
    await new Promise(resolve => setTimeout(resolve, 70))
    if (init?.signal?.aborted) throw new DOMException('Aborted', 'AbortError')
    const url = new URL(String(input), 'http://synthetic.local'); const path = url.pathname.replace('/api/v1/', '')
    const method = init?.method ?? 'GET'
    if (path === 'auth/session' || path === 'auth/exchange') return json({ csrf_token: 'synthetic-csrf-only' })
    if (method !== 'GET' && new Headers(init?.headers).get('X-CSRF-Token') !== 'synthetic-csrf-only') return json({ error: { code: 'CSRF_INVALID', message: '请重新打开管理页面。' } }, 403)
    if (path === 'auth/renew') return json({ csrf_token: 'synthetic-csrf-only' })
    if (path === 'capabilities') return json({ api_version: '1.8', protocol_status: 'experimental', transports: ['http-sse'] })
    if (path === 'activity') return json({ received: 0, items: [], degraded: false })
    if (path === 'requests') return json({ items: [], total: 0, next_cursor: '', degraded: false, page: 1, total_pages: 0, snapshot: 'synthetic-empty' })
    if (path === 'history/policy') { if (method === 'PUT') retentionDays = parse<{ days: number }>(init).days; return json({ days: retentionDays }) }
    if (path === 'history/clear') return json({ deleted: 0 })
    if (path === 'sessions/page') { const kind = url.searchParams.get('kind') ?? 'bound'; const limit = Number(url.searchParams.get('limit') ?? 20); const offset = Number(url.searchParams.get('cursor') ?? 0); const rows = kind === 'bound' ? sessions.bindings : sessions.pending; return json({ bindings: kind === 'bound' ? rows.slice(offset, offset + limit) : [], pending: kind === 'pending' ? rows.slice(offset, offset + limit) : [], kind, total: rows.length, next_cursor: rows.length > offset + limit ? String(offset + limit) : '' }) }
    if (path === 'daily') return json(daily)
    if (path === 'daily/preview') { dailyAction = parse<{action: string}>(init).action; dailyToken = `synthetic-preview-${++sequence}`; return json({ token: dailyToken, action: dailyAction, config_path: daily.config_path, before: daily.has_backup ? daily.base_url : '未设置', after: dailyAction === 'route' ? daily.base_url : '移除连接字段', expires_at: new Date(Date.now() + 300000).toISOString(), warning: '合成预览。只修改连接字段，请手动重开客户端。' }) }
    if (path === 'daily/apply') { if (parse<{token: string}>(init).token !== dailyToken || !dailyToken) return json({ error: { code: 'DAILY_PREVIEW_EXPIRED', message: '请重新预览' } }, 409); daily = { ...daily, state: dailyAction === 'route' ? 'configured' : 'direct', has_backup: dailyAction === 'route', message: '合成配置已更新，真实配置未改' }; dailyToken = ''; return json(daily) }
    if (path === 'clients') return json({ items: [
      { target: 'cli', installed: true, version: 'codex-cli 0.166.0', state: 'ready', mode: 'routed', profile: 'C:/Synthetic/CodexRouter/cli', message: '专用测试环境，尚未登录' },
      { target: 'desktop', installed: true, version: '26.1029.7803.0', state: 'ready', mode: 'routed', profile: 'C:/Synthetic/CodexRouter/desktop', message: '独立桌面环境，真实兼容性待验收' },
    ] })
    if (path === 'clients/action') return json({ items: [] })
    if (path === 'config') {
      if (method === 'PUT') config = { ...parse<Omit<Config, 'version'>>(init), version: 2 }
      return json(config)
    }
    if (path === 'status') return json({ local: 'running', active_requests: 0, upstreams: providers.map(p => ({ provider_id: p.id, result: 'unverified', checked_at: '', code: '' })) })
    if (path === 'providers') {
      if (method === 'POST') {
        sequence += 1
        const created: Provider = { id: `p-synthetic-${sequence}`, ...parse<ProviderCreateInput>(init), key_configured: false, upstream_locked: false }
        providers = [...providers, created]
        return json(created)
      }
      return json({ items: providers })
    }
    if (path === 'providers/models') return json({ items: [{ id: 'gpt-demo' }, { id: 'gpt-demo-fast' }] })
    if (path === 'providers/save') {
      const input = parse<ProviderSaveInput>(init)
      let provider = providers.find(p => p.id === input.provider_id)
      if (!provider) {
        provider = { id: `p-synthetic-${++sequence}`, name: input.name, kind: 'api_key', base_url: input.base_url, model: input.model, key_configured: true, upstream_locked: false }
        providers.push(provider)
      } else Object.assign(provider, { name: input.name, base_url: input.base_url, model: input.model, key_configured: true })
      keys.add(provider.id)
      return json(provider)
    }
    if (path.startsWith('providers/')) {
      const id = decodeURIComponent(path.slice('providers/'.length))
      const provider = providers.find(x => x.id === id)
      if (!provider) return json({ error: { code: 'PROVIDER_NOT_FOUND', message: '供应商不存在。' } }, 404)
      if (method === 'DELETE') {
        if (id === 'official' || provider.upstream_locked) return json({ error: { code: 'PROVIDER_LOCKED', message: '不能删除该供应商。' } }, 409)
        providers = providers.filter(x => x.id !== id)
        return json({ items: providers })
      }
      const body = parse<ProviderUpdateInput>(init)
      providers = providers.map(x => x.id === id ? { ...x, ...body } : x)
      return json({ ...provider, ...body })
    }
    if (path === 'credentials') {
      return json({ items: providers.filter(x => x.kind === 'api_key').map(x => ({ provider_id: x.id, configured: keys.has(x.id) })) })
    }
    if (path.startsWith('credentials/')) {
      const id = decodeURIComponent(path.slice('credentials/'.length))
      if (!providers.some(x => x.id === id)) return json({ error: { code: 'PROVIDER_NOT_FOUND', message: '供应商不存在。' } }, 404)
      if (method === 'PUT') keys.add(id)
      if (method === 'DELETE') keys.delete(id)
      return json({ provider_id: id, configured: keys.has(id) })
    }
    if (path === 'sessions') return json({ bindings: sessions.bindings, pending: sessions.pending })
    if (path === 'sessions/register') {
      const value = parse<RegisterInput>(init)
      const pending = sessions.pending.findIndex(x => x.id === value.id)
      if (pending < 0) return json({ error: { code: 'SESSION_NOT_PENDING', message: '此会话不在待登记列表。' } }, 409)
      sessions.pending.splice(pending, 1)
      const binding = { ...value, created_at: new Date().toISOString() }
      sessions.bindings.push(binding)
      return json(binding)
    }
    if (path === 'diagnostics') return json({ items: diagnostics })
    return json({ error: { code: 'NOT_FOUND', message: '接口不存在。' } }, 404)
  }
}
