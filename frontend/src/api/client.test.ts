import { describe, expect, it, vi } from 'vitest'
import { ApiClient, ApiError, assertCompatible, consumeBootstrap } from './client'
import type { FetchTransport } from './client'
import { createMockApi } from '../mock'

describe('管理 API 边界', () => {
  it('仅续期在明确CSRF失配时读取共享会话并重试一次', async () => {
    const calls: { path: string; csrf: string | null }[] = []
    let renewed = 0
    const client = new ApiClient(async (input, init) => {
      const path = String(input); calls.push({ path, csrf: new Headers(init?.headers).get('X-CSRF-Token') })
      if (path.endsWith('auth/renew') && ++renewed === 1) return new Response(JSON.stringify({ error: { code: 'CSRF_REQUIRED' } }), { status: 403 })
      return new Response(JSON.stringify({ csrf_token: 'synthetic-refreshed' }))
    })
    await client.renew()
    expect(calls.map(x => x.path)).toEqual(['/api/v1/auth/renew', '/api/v1/auth/session', '/api/v1/auth/renew'])
    expect(calls[2].csrf).toBe('synthetic-refreshed')
  })

  it('失效认证和普通业务写入均不自动重试，续期重试失败直接返回', async () => {
    for (const status of [401, 403]) {
      const transport = vi.fn(async () => new Response(JSON.stringify({ error: { code: status === 401 ? 'AUTH_REQUIRED' : 'CSRF_REQUIRED' } }), { status }))
      const client = new ApiClient(transport)
      await expect(client.saveKey('company', 'synthetic')).rejects.toMatchObject({ status })
      expect(transport).toHaveBeenCalledTimes(1)
      transport.mockClear()
      await expect(client.renew()).rejects.toMatchObject({ status })
      expect(transport).toHaveBeenCalledTimes(status === 401 ? 1 : 2)
    }
    let attempts = 0
    const client = new ApiClient(async input => {
      if (String(input).endsWith('auth/session')) return new Response(JSON.stringify({ csrf_token: 'synthetic' }))
      attempts++
      return new Response(JSON.stringify({ error: { code: 'CSRF_REQUIRED' } }), { status: 403 })
    })
    await expect(client.renew()).rejects.toMatchObject({ status: 403 })
    expect(attempts).toBe(2)
  })

  it('默认传输保留浏览器 fetch 的全局接收者', async () => {
    vi.stubGlobal('fetch', function (this: unknown) {
      if (this !== globalThis) throw new TypeError('Illegal invocation')
      return Promise.resolve(new Response(JSON.stringify({ items: [] })))
    })
    try { expect(await new ApiClient().credentials()).toEqual({ items: [] }) }
    finally { vi.unstubAllGlobals() }
  })

  it('消费 fragment 后立即清除令牌，不写入查询参数', () => {
    const replaceState = vi.fn()
    expect(consumeBootstrap({ hash: '#token=synthetic-one-use', pathname: '/', search: '?view=overview' }, { replaceState })).toBe('synthetic-one-use')
    expect(replaceState).toHaveBeenCalledWith(null, '', '/?view=overview')
    replaceState.mockClear()
    expect(consumeBootstrap({ hash: '', pathname: '/', search: '' }, { replaceState })).toBeNull()
    expect(replaceState).not.toHaveBeenCalled()
  })

  it('拒绝不兼容的主版本或旧次版本', () => {
    expect(() => assertCompatible({ api_version: '2.0', protocol_status: 'experimental', transports: [] })).toThrow('不兼容')
    expect(() => assertCompatible({ api_version: '10.0', protocol_status: 'experimental', transports: [] })).toThrow('不兼容')
    expect(() => assertCompatible({ api_version: '1.0', protocol_status: 'experimental', transports: [] })).toThrow('不兼容')
    expect(() => assertCompatible({ api_version: '1.2', protocol_status: 'experimental', transports: [] })).toThrow('不兼容')
    expect(() => assertCompatible({ api_version: '1.3', protocol_status: 'experimental', transports: [] })).toThrow('不兼容')
    expect(() => assertCompatible({ api_version: '1.7', protocol_status: 'experimental', transports: [] })).not.toThrow()
    expect(() => assertCompatible({ api_version: '1.6', protocol_status: 'experimental', transports: [] })).toThrow('不兼容')
  })

  it('拒绝缺少用量契约的旧版本', () => { expect(() => assertCompatible({ api_version: '1.5', protocol_status: 'experimental', transports: [] })).toThrow('不兼容') })

  it('认证用 cookie，写入使用内存 CSRF，读取不携带 CSRF', async () => {
    const calls: RequestInit[] = []
    const mock = createMockApi()
    const client = new ApiClient(async (input, init) => { calls.push(init!); return mock(input, init) })
    await client.authenticate('synthetic')
    await client.saveKey('company', 'synthetic-company-key')
    await client.credentials()
    expect(calls[0].credentials).toBe('same-origin')
    expect(new Headers(calls[0].headers).has('X-CSRF-Token')).toBe(false)
    expect(new Headers(calls[1].headers).get('X-CSRF-Token')).toBe('synthetic-csrf-only')
    expect(new Headers(calls[2].headers).has('X-CSRF-Token')).toBe(false)
  })

  it('保存失败保留服务端错误码，不将失败当成功', async () => {
    const client = new ApiClient(async () => new Response(JSON.stringify({ error: { code: 'STORE_WRITE_FAILED', message: '持久化失败。' } }), { status: 500 }))
    await expect(client.config()).rejects.toMatchObject({ code: 'STORE_WRITE_FAILED', status: 500, message: '持久化失败。' })
  })

  it('非 JSON 网关错误有可读反馈', async () => {
    const client = new ApiClient(async () => new Response('<html>failure</html>', { status: 502 }))
    await expect(client.status()).rejects.toMatchObject({ code: 'HTTP_ERROR', status: 502 })
  })

  it('后端不可达返回明确错误', async () => {
    const client = new ApiClient(async () => { throw new TypeError('network failed') })
    await expect(client.status()).rejects.toMatchObject({ code: 'BACKEND_UNREACHABLE' })
  })

  it('保留调用方取消，不误报断网', async () => {
    const controller = new AbortController()
    const transport: FetchTransport = async () => { controller.abort(); throw new DOMException('cancelled', 'AbortError') }
    const client = new ApiClient(transport)
    await expect(client.status(controller.signal)).rejects.not.toBeInstanceOf(ApiError)
  })

  it('合成后端支持供应商增删、首次登记与独立密钥状态', async () => {
    const client = new ApiClient(createMockApi())
    await client.authenticate(null)
    const before = await client.sessions()
    const id = before.pending[0].id
    await client.register({ id, provider_id: 'official' })
    const after = await client.sessions()
    expect(after.pending).toHaveLength(0)
    expect(after.bindings.some(x => x.id === id && x.provider_id === 'official')).toBe(true)
    await expect(client.register({ id, provider_id: 'company' })).rejects.toMatchObject({ status: 409 })

    const created = await client.createProvider({ name: '合成线路', kind: 'api_key', base_url: 'https://api.example.test/v1', model: 'gpt-test' })
    expect(created.key_configured).toBe(false)
    const updated = await client.updateProvider(created.id, { name: '合成线路 2', base_url: created.base_url, model: '' })
    expect(updated.name).toBe('合成线路 2')
    await client.saveKey(created.id, 'synthetic-company-key')
    expect((await client.credentials()).items.find(x => x.provider_id === created.id)).toEqual({ provider_id: created.id, configured: true })
    await client.deleteKey(created.id)
    expect((await client.credentials()).items.find(x => x.provider_id === created.id)).toEqual({ provider_id: created.id, configured: false })
    const removed = await client.deleteProvider(created.id)
    expect(removed.items.some(x => x.id === created.id)).toBe(false)

    const saved = await client.saveConfig({ default_provider_id: 'official', connect_timeout_ms: 12000, header_timeout_ms: 30000, idle_timeout_ms: 120000, write_timeout_ms: 30000, max_concurrent: 16 })
    expect(saved.default_provider_id).toBe('official')
    expect(saved.version).toBe(2)
  })
})
