import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'
import { writeFileSync } from 'node:fs'
import type { Config, Provider, Sessions, Client, DailyStatus, Activity, RequestActivity } from '../src/api/types.gen'

function syntheticRequest(input: Partial<RequestActivity> & Pick<RequestActivity, 'id' | 'time' | 'entry' | 'thread_id' | 'provider_id' | 'phase' | 'result' | 'status' | 'elapsed_ms'>): RequestActivity {
  const outcome = input.outcome ?? (input.phase === 'received' ? 'processing' : input.result.startsWith('HTTP_2') ? 'success' : 'error')
  return { observation: { terminal_result: outcome === 'success' ? 'RESPONSE_COMPLETED' : 'unknown', transport_result: 'EOF', metrics_status: 'available' }, outcome, model: 'gpt-synthetic', request_type: 'stream', error_stage: outcome === 'error' ? 'stream' : '', metrics: { input_tokens: 144746, output_tokens: 4413, total_tokens: 149159, cache_read_tokens: 144400, cache_write_tokens: 0, reasoning_tokens: 400, first_token_ms: 6490, protocol_completed_ms: 68000, known_fields: ['input_tokens', 'output_tokens', 'total_tokens', 'cache_read_tokens', 'cache_write_tokens', 'reasoning_tokens', 'first_token_ms', 'protocol_completed_ms'] }, ...input }
}

async function navigate(page: Page, label: string) {
  const groups: Record<string, string> = { '请求明细': '请求管理', '历史管理': '请求管理', '待登记会话': '会话管理', '已绑定会话': '会话管理' }
  const target = page.getByRole('button', { name: label, exact: true })
  if (groups[label] && !await target.isVisible()) await page.getByRole('button', { name: groups[label], exact: true }).click()
  await target.click()
}

async function backend(page: Page) {
  const state = {
    failConfig: false,
    failHistory: false,
    failClear: false,
    failSessions: false,
    historyDays: 0,
    failModels: false,
    failProviderSave: false,
    disconnected: false,
    version: '1.8',
    dailyFail: false,
    dailyAction: 'route',
    daily: { state: 'direct', config_path: 'C:/Synthetic/daily/config.toml', base_url: 'http://127.0.0.1:18788', has_backup: false, message: '原登录、历史与项目保持' } as DailyStatus,
    activity: { received: 0, items: [], degraded: false } as Activity,
    diagnostics: { items: [{ time: '2026-11-03T12:35:00Z', code: 'SESSION_PENDING', provider_id: 'official' }] },
    authenticated: true,
    clients: [
      { target: 'cli', installed: true, version: 'synthetic-cli', state: 'ready', mode: 'routed', profile: 'C:/Synthetic/cli', message: '合成专用客户端' },
      { target: 'desktop', installed: true, version: 'synthetic-desktop', state: 'ready', mode: 'routed', profile: 'C:/Synthetic/desktop', message: '合成专用桌面' },
    ] as Client[],
    config: { version: 2, default_provider_id: 'official', connect_timeout_ms: 10000, header_timeout_ms: 30000, idle_timeout_ms: 120000, write_timeout_ms: 30000, max_concurrent: 16 } as Config,
    providers: [
      { id: 'official', name: '官方线路', kind: 'official', base_url: 'https://chatgpt.com/backend-api/codex', model: '', key_configured: false, upstream_locked: true },
      { id: 'company', name: '公司线路', kind: 'api_key', base_url: 'https://example.test/v1', model: 'gpt-company', key_configured: true, upstream_locked: false },
    ] as Provider[],
    sessions: { bindings: [{ id: 'isolated-official-session', provider_id: 'official', created_at: '2026-11-03T12:30:00Z' }], pending: [{ id: 'isolated-pending-session', first_seen: '2026-11-03T12:35:00Z', reason: 'unknown_origin' }] } as Sessions,
  }
  await page.route('**/api/v1/**', async route => {
    const request = route.request()
    const path = new URL(request.url()).pathname.replace('/api/v1/', '')
    const method = request.method()
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (state.disconnected) return route.abort('connectionrefused')
    if (!state.authenticated) return json({ error: { code: 'UNAUTHORIZED', message: '管理会话失效。' } }, 401)
    if (path === 'auth/exchange' || path === 'auth/session') return json({ csrf_token: 'synthetic-csrf' })
    if (method !== 'GET' && request.headers()['x-csrf-token'] !== 'synthetic-csrf') return json({ error: { code: 'CSRF_INVALID', message: 'CSRF 校验失败。' } }, 403)
    if (path === 'capabilities') return json({ api_version: state.version, protocol_status: 'experimental', transports: ['http-sse'] })
    if (path === 'auth/renew') return json({ csrf_token: 'synthetic-csrf' })
    if (path === 'daily') return json(state.daily)
    if (path === 'activity') return json(state.activity)
    const query = new URL(request.url()).searchParams
    if (path === 'requests') {
      if (state.failHistory) return json({ error: { code: 'HISTORY_UNAVAILABLE', message: '历史暂不可用，请重试' } }, 503)
      const filter = query.get('filter') ?? 'all'; const rows = state.activity.items.filter(item => filter === 'all' || (filter === 'normal' ? item.phase === 'finished' && item.result.startsWith('HTTP_2') : item.outcome === filter))
      const size = Number(query.get('limit') || 20); const pageNumber = Math.min(Number(query.get('page') || 1), Math.max(1, Math.ceil(rows.length / size))); const offset = (pageNumber - 1) * size
      return json({ items: rows.slice(offset, offset + size), total: rows.length, next_cursor: '', degraded: state.activity.degraded, page: pageNumber, total_pages: Math.ceil(rows.length / size), snapshot: 'synthetic-snapshot' })
    }
    if (path === 'history/policy') { if (method === 'PUT') state.historyDays = request.postDataJSON().days; return json({ days: state.historyDays }) }
    if (path === 'history/clear') { if (state.failClear) return json({ error: { code: 'HISTORY_UNAVAILABLE', message: '清理未完成，请重试' } }, 503); const before = state.activity.items.length; state.activity.items = state.activity.items.filter(item => item.phase === 'received'); return json({ deleted: before - state.activity.items.length }) }
    if (path === 'sessions/page') { if (state.failSessions) return json({ error: { code: 'HISTORY_UNAVAILABLE', message: '会话索引暂不可用' } }, 503); const kind = query.get('kind') ?? 'bound'; const limit = Number(query.get('limit') || 20); const offset = Number(query.get('cursor') || 0); const rows = kind === 'bound' ? state.sessions.bindings : state.sessions.pending; return json({ kind, bindings: kind === 'bound' ? rows.slice(offset, offset + limit) : [], pending: kind === 'pending' ? rows.slice(offset, offset + limit) : [], total: rows.length, next_cursor: offset + limit < rows.length ? String(offset + limit) : '' }) }
    if (path === 'daily/preview') { state.dailyAction = (request.postDataJSON() as { action: string }).action; return json({ token: 'synthetic-daily-preview', action: state.dailyAction, config_path: state.daily.config_path, before: '未设置', after: state.dailyAction === 'route' ? state.daily.base_url : '移除接入字段', expires_at: new Date(Date.now() + 300000).toISOString(), warning: '请保存工作并手动重开 Codex，不改登录和历史。' }) }
    if (path === 'daily/apply') {
      if (state.dailyFail) return json({ error: { code: 'DAILY_CONFIG_CHANGED', message: '预览后配置发生变化，请重新预览；未覆盖外部修改' } }, 409)
      state.daily.state = state.dailyAction === 'route' ? 'configured' : 'direct'; state.daily.has_backup = state.dailyAction === 'route'; return json(state.daily)
    }
    if (path === 'clients') return json({ items: state.clients })
    if (path === 'clients/action') {
      const input = request.postDataJSON() as { target: string; action: string }
      state.clients = state.clients.map(item => item.target !== input.target ? item : { ...item, state: input.action === 'start' ? 'running' : 'ready', mode: input.action === 'direct' ? 'direct' : input.action === 'routed' ? 'routed' : item.mode })
      return json({ items: state.clients })
    }
    if (path === 'config') {
      if (method === 'PUT') {
        if (state.failConfig) return json({ error: { code: 'STORE_WRITE_FAILED', message: '保存失败：无法写入配置。请检查数据目录。' } }, 500)
        state.config = { ...request.postDataJSON() as Config, version: 2 }
      }
      return json(state.config)
    }
    if (path === 'status') return json({ local: 'running', active_requests: 0, upstreams: state.providers.map(item => ({
      provider_id: item.id,
      result: item.id === 'official' ? 'success' : 'unverified',
      checked_at: item.id === 'official' ? '2026-11-03T12:30:00Z' : '',
      code: item.id === 'official' ? 'HTTP_200' : '',
    })) })
    if (path === 'providers') {
      if (method === 'POST') {
        const input = request.postDataJSON() as Pick<Provider, 'name' | 'kind' | 'base_url' | 'model'>
        const created: Provider = { id: 'p-synthetic', ...input, key_configured: false, upstream_locked: false }
        state.providers.push(created)
        return json(created)
      }
      return json({ items: state.providers })
    }
    if (path === 'providers/models') {
      if (state.failModels) return json({ error: { code: 'MODEL_QUERY_UNSUPPORTED', message: '该地址不提供模型列表，可手动填写模型后保存' } }, 502)
      return json({ items: [{ id: 'gpt-test-a' }, { id: 'gpt-test-b' }] })
    }
    if (path === 'providers/save') {
      if (state.failProviderSave) return json({ error: { code: 'STORAGE_WRITE_FAILED', message: '供应商未保存；原配置仍有效' } }, 500)
      const input = request.postDataJSON() as { provider_id: string; name: string; base_url: string; model: string; key: string }
      let provider = state.providers.find(p => p.id === input.provider_id)
      if (!provider) { provider = { id: 'p-synthetic', kind: 'api_key', name: input.name, base_url: input.base_url, model: input.model, key_configured: true, upstream_locked: false }; state.providers.push(provider) }
      else Object.assign(provider, { name: input.name, base_url: input.base_url, model: input.model, key_configured: true })
      return json(provider)
    }
    if (path.startsWith('providers/')) {
      const id = path.slice('providers/'.length)
      const provider = state.providers.find(x => x.id === id)
      if (!provider) return json({ error: { code: 'PROVIDER_NOT_FOUND', message: '供应商不存在。' } }, 404)
      if (method === 'DELETE') {
        state.providers = state.providers.filter(x => x.id !== id)
        return json({ items: state.providers })
      }
      Object.assign(provider, request.postDataJSON())
      return json(provider)
    }
    if (path === 'credentials') return json({ items: state.providers.filter(x => x.kind === 'api_key').map(x => ({ provider_id: x.id, configured: x.key_configured })) })
    if (path.startsWith('credentials/')) {
      const id = path.slice('credentials/'.length)
      const provider = state.providers.find(x => x.id === id)
      if (!provider) return json({ error: { code: 'PROVIDER_NOT_FOUND', message: '供应商不存在。' } }, 404)
      provider.key_configured = method === 'PUT'
      return json({ provider_id: id, configured: provider.key_configured })
    }
    if (path === 'sessions') return json(state.sessions)
    if (path === 'sessions/register') {
      const { id, provider_id } = request.postDataJSON() as { id: string; provider_id: string }
      state.sessions.pending = state.sessions.pending.filter(x => x.id !== id)
      const binding = { id, provider_id, created_at: '2026-11-03T12:36:00Z' }
      state.sessions.bindings.push(binding)
      return json(binding)
    }
    if (path === 'diagnostics') return json(state.diagnostics)
    return json({ error: { code: 'NOT_FOUND', message: '不存在。' } }, 404)
  })
  return state
}

test('成功错误独立、用量格式和键盘明细、多视口复核', async ({ page }) => {
  const state = await backend(page)
  state.activity = { received: 9, degraded: false, items: Array.from({ length: 9 }, (_, i) => syntheticRequest({ id: i + 1, time: '2026-11-06T01:30:00Z', entry: 'POST /responses', thread_id: `synthetic-usage-session-${i + 1}`, provider_id: i % 2 ? 'official' : 'company', phase: i === 7 ? 'received' : 'finished', outcome: i < 6 ? 'success' : i === 6 ? 'error' : i === 7 ? 'processing' : 'unconfirmed', result: i < 6 ? 'RESPONSE_COMPLETED' : i === 6 ? 'RESPONSE_FAILED' : i === 7 ? '处理中' : 'HTTP_200', status: i === 7 ? 0 : 200, elapsed_ms: 70000 + i * 1000 })) }
  await page.goto('/')
  await navigate(page, '请求明细')
  const region = page.getByRole('region', { name: '请求明细，可横向滚动' })
  await expect(region.locator('tbody tr')).toHaveCount(6)
  await expect(region.getByText('346', { exact: true })).toHaveCount(6)
  await expect(region.locator('.token-cache')).toHaveText(Array(6).fill('缓存命中 144.4K'))
  await expect(region.getByText('6.49s', { exact: true })).toHaveCount(6)
  await expect(region.getByText('1m 8s', { exact: true })).toHaveCount(6)
  await expect(region.getByText('64.9 tok/s', { exact: true })).toHaveCount(6)
  await expect(region.locator('.latency-bar > :first-child')).toHaveClass(Array(6).fill('severity-good'))
  await expect(region.locator('.latency-bar > :last-child')).toHaveClass(Array(6).fill('severity-warn'))
  const usageButton = page.getByRole('button', { name: '查看请求 1 用量明细', exact: true })
  await usageButton.focus(); await page.keyboard.press('Enter')
  const details = page.getByRole('dialog', { name: '请求用量明细', exact: true })
  await expect(details).toBeVisible()
  await expect(details.getByText('144,400', { exact: true })).toBeVisible()
  await expect(details.getByText('400', { exact: true })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(details).not.toBeVisible()
  for (const viewport of [{ width: 1440, height: 900 }, { width: 2552, height: 1274 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport)
    for (const [tab, outcome] of [['用量明细', 'success'], ['错误请求', 'error'], ['处理中', 'processing'], ['待确认', 'unconfirmed']] as const) {
      await page.getByRole('tab', { name: tab, exact: true }).click()
      await expect(region.locator('tbody tr[data-outcome]')).toHaveCount(outcome === 'success' ? 6 : 1)
      expect(await region.locator('tbody tr[data-outcome]').evaluateAll((rows, expected) => rows.every(row => row.getAttribute('data-outcome') === expected), outcome)).toBe(true)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      await page.screenshot({ path: `test-results/final-${viewport.width}-requests-${outcome}.png`, fullPage: true, animations: 'disabled' })
      if (viewport.width === 390 && outcome === 'success') {
        const mobileDetails = region.getByRole('button', { name: '查看请求 1 用量明细', exact: true })
        await mobileDetails.scrollIntoViewIfNeeded()
        await expect(mobileDetails).toBeInViewport()
        await page.screenshot({ path: 'test-results/final-390-requests-scrolled.png', fullPage: true, animations: 'disabled' })
        await mobileDetails.click()
        await expect(details).toBeVisible()
        await expect(details.getByText('144,400', { exact: true })).toBeVisible()
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
        await page.screenshot({ path: 'test-results/final-390-requests-detail.png', fullPage: false, animations: 'disabled' })
        await details.getByRole('button', { name: '关闭明细', exact: true }).click()
        await region.evaluate(element => { element.scrollLeft = 0 })
      }
    }
  }
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.getByRole('tab', { name: '用量明细', exact: true }).click()
  await usageButton.click()
  await page.screenshot({ path: 'test-results/final-1440-requests-detail.png', fullPage: false, animations: 'disabled' })
  await page.keyboard.press('Escape')
  await page.getByRole('tab', { name: '用量明细', exact: true }).focus()
  await page.keyboard.press('ArrowRight')
  await expect(page.getByRole('tab', { name: '错误请求', exact: true })).toBeFocused()
  await expect(region.getByText('上游生成失败', { exact: true })).toBeVisible()
})

test('切换标签立即清空旧分类，迟到响应不得覆盖新分类', async ({ page }) => {
  const state = await backend(page)
  state.activity = { received: 2, degraded: false, items: [syntheticRequest({ id: 1, time: '2026-11-06T01:30:00Z', entry: 'POST /responses', thread_id: 'only-success', provider_id: 'company', phase: 'finished', result: 'RESPONSE_COMPLETED', outcome: 'success', status: 200, elapsed_ms: 1000 }), syntheticRequest({ id: 2, time: '2026-11-06T01:30:00Z', entry: 'POST /responses', thread_id: 'only-error', provider_id: 'company', phase: 'finished', result: 'RESPONSE_FAILED', outcome: 'error', status: 200, elapsed_ms: 1000 })] }
  let release: (() => void) | undefined
  await page.route('**/api/v1/requests?**', async route => {
    if (new URL(route.request().url()).searchParams.get('filter') === 'error') await new Promise<void>(resolve => { release = resolve })
    await route.fallback()
  })
  await page.goto('/')
  await navigate(page, '请求明细')
  await expect(page.getByText('only-success', { exact: true })).toBeVisible()
  await page.getByRole('tab', { name: '错误请求', exact: true }).click()
  await expect(page.getByText('only-success', { exact: true })).toHaveCount(0)
  await expect.poll(() => !!release).toBe(true)
  await page.getByRole('tab', { name: '处理中', exact: true }).click()
  release!()
  await expect(page.getByText('暂无匹配的请求记录', { exact: true })).toBeVisible()
  await expect(page.getByText('only-error', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('tab', { name: '处理中', exact: true })).toHaveAttribute('aria-selected', 'true')
})

test('仅数据区域滚动且列对齐，页码跳转和历史管理独立', async ({ page }) => {
  const state = await backend(page)
  state.activity = { received: 121, degraded: false, items: Array.from({ length: 121 }, (_, i) => syntheticRequest({ id: 121 - i, time: '2026-01-02T03:04:05.123+08:00', entry: 'POST /responses', thread_id: `scroll-page-session-${i}`, provider_id: 'company', phase: 'finished', outcome: 'success', result: 'RESPONSE_COMPLETED', status: 200, elapsed_ms: 70000 })) }
  await page.goto('/')
  await navigate(page, '请求明细')
  const main = page.locator('main')
  await expect(page.getByRole('region', { name: '请求明细，可横向滚动' }).locator('tbody tr')).toHaveCount(20)
  for (const viewport of [{ width: 1440, height: 900 }, { width: 2552, height: 1274 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport)
    const region = page.getByRole('region', { name: '请求明细，可横向滚动' })
    await region.evaluate(el => { el.scrollTop = 0; el.scrollLeft = 0 })
    const headingBefore = await page.locator('.page-heading').boundingBox()
    const tabsBefore = await page.locator('.request-controls').boundingBox()
    const paginationBefore = await page.locator('.request-pagination').boundingBox()
    const tableHeaderBefore = await region.locator('thead').boundingBox()
    const headerBefore = await page.locator('.topbar').boundingBox()
    const navBefore = await page.locator('.sidebar').boundingBox()
    await region.evaluate(el => { el.scrollTop = el.scrollHeight })
    expect(await page.evaluate(() => window.scrollY)).toBe(0)
    expect(await main.evaluate(el => el.scrollTop)).toBe(0)
    expect(await region.evaluate(el => el.scrollTop)).toBeGreaterThan(0)
    expect(await page.locator('.page-heading').boundingBox()).toEqual(headingBefore)
    expect(await page.locator('.request-controls').boundingBox()).toEqual(tabsBefore)
    expect(await page.locator('.request-pagination').boundingBox()).toEqual(paginationBefore)
    expect(await region.locator('thead').boundingBox()).toEqual(tableHeaderBefore)
    const alignment = await region.locator('tbody tr').first().evaluate(row => Array.from(row.querySelectorAll('td')).map(td => { const cell = td.getBoundingClientRect(); const block = td.querySelector('.request-cell')?.getBoundingClientRect(); return block ? Math.abs((cell.left + cell.right - block.left - block.right) / 2) : 0 }))
    expect(Math.max(...alignment)).toBeLessThan(2)
    const centeredText = await region.locator('tbody tr').first().evaluate(row => Array.from(row.querySelectorAll('td')).map(td => getComputedStyle(td).textAlign))
    expect(centeredText).toEqual(Array(8).fill('center'))
    expect(await page.locator('.request-pagination').evaluate(el => innerHeight - el.getBoundingClientRect().bottom)).toBeLessThan(24)
    expect(await page.locator('.topbar').boundingBox()).toEqual(headerBefore)
    expect(await page.locator('.sidebar').boundingBox()).toEqual(navBefore)
    expect(await page.evaluate(() => document.documentElement.scrollHeight <= innerHeight && document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await expect(page.getByLabel('跳至页码')).toBeInViewport()
    await page.screenshot({ path: `test-results/final-${viewport.width}-content-scroll.png`, animations: 'disabled' })
  }
  await page.getByLabel('跳至页码').fill('7')
  await page.getByRole('button', { name: '跳转', exact: true }).click()
  await expect(page.getByRole('button', { name: '第 7 页', exact: true })).toHaveAttribute('aria-current', 'page')
  await expect(page.getByText('scroll-page-session-120', { exact: true })).toBeVisible()
  await navigate(page, '历史管理')
  await expect(page.getByRole('region', { name: '历史管理', exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: '请求明细，可横向滚动' })).toHaveCount(0)
  for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport)
    await page.getByLabel('保留天数').scrollIntoViewIfNeeded()
    await page.screenshot({ path: `test-results/final-${viewport.width}-history-management.png`, animations: 'disabled' })
  }
})

test('可见页自动续期、隐藏页暂停、失效时明确托盘恢复', async ({ page }) => {
  await page.clock.install()
  const state = await backend(page)
  let renewals = 0
  page.on('request', request => { if (request.url().endsWith('/api/v1/auth/renew')) renewals++ })
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await page.clock.fastForward(5 * 60_000)
  await expect.poll(() => renewals).toBe(1)
  await page.evaluate(() => { Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' }); document.dispatchEvent(new Event('visibilitychange')) })
  await page.clock.fastForward(8 * 3600_000)
  expect(renewals).toBe(1)
  state.authenticated = false
  await page.evaluate(() => { Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' }); document.dispatchEvent(new Event('visibilitychange')) })
  await expect(page.getByText('管理认证已失效', { exact: true })).toBeVisible()
  await expect(page.getByText(/请从托盘重新打开管理页面/)).toBeVisible()
  await page.clock.fastForward(10 * 60_000)
  expect(renewals).toBe(2)
})

test('桌面总览、保存失败与首次登记', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  const state = await backend(page)
  await page.goto('/#token=synthetic-bootstrap')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await expect(page.getByRole('button', { name: '刷新', exact: true })).toBeEnabled()
  expect(new URL(page.url()).hash).toBe('')
  await expect(page.getByText('最近成功', { exact: true })).toBeVisible()
  await page.screenshot({ path: 'test-results/desktop-overview.png', fullPage: true, animations: 'disabled' })
  await page.getByRole('button', { name: /中转模式/ }).click()
  await page.getByLabel('中转供应商', { exact: true }).selectOption('company')
  state.failConfig = true
  await page.getByRole('button', { name: '保存并应用' }).click()
  await expect(page.getByText('保存失败：无法写入配置。请检查数据目录。')).toBeVisible()
  expect(state.config.default_provider_id).toBe('official')
  state.failConfig = false
  await page.getByRole('button', { name: '保存并应用' }).click()
  await expect(page.getByText('已保存新会话默认供应商。已有会话保持原绑定。')).toBeVisible()
  await navigate(page, '待登记会话')
  await page.getByLabel('首次供应商').selectOption('official')
  await page.getByRole('button', { name: '确认供应商' }).click()
  await expect(page.getByText('请在客户端重试原请求', { exact: false })).toBeVisible()
  await expect(page.getByText('没有等待登记的会话')).toBeVisible()
  await navigate(page, '已绑定会话')
  await expect(page.getByRole('cell', { name: 'isolated-pending-session', exact: true })).toBeVisible()
})

test('手机布局、键盘导航与密钥不回显', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await backend(page)
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await expect(page.getByRole('button', { name: '刷新', exact: true })).toBeEnabled()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: 'test-results/mobile-overview.png', fullPage: true, animations: 'disabled' })
  await page.getByRole('button', { name: '供应商设置' }).focus()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('heading', { name: '中转供应商' })).toBeVisible()
  await page.getByRole('button', { name: '编辑 公司线路', exact: true }).click()
  await expect(page.getByLabel('BaseUrl', { exact: false }).first()).toBeEnabled()
  await page.getByLabel('API Key', { exact: false }).first().fill('synthetic-browser-key')
  await page.getByRole('button', { name: '保存供应商' }).first().click()
  await expect(page.getByText('供应商已保存。默认模型对下一条请求生效。')).toBeVisible()
  await page.getByRole('button', { name: '编辑 公司线路', exact: true }).click()
  await expect(page.getByLabel('API Key', { exact: false }).first()).toHaveValue('')
  await page.getByRole('button', { name: '关闭编辑', exact: true }).click()
  expect(await page.evaluate(() => ({ local: localStorage.length, session: sessionStorage.length }))).toEqual({ local: 0, session: 0 })
  expect(await page.content()).not.toContain('synthetic-browser-key')
  await expect(page.getByRole('button', { name: '保存超时与并发' })).toBeEnabled()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.evaluate(() => scrollTo(0, 0))
  await page.screenshot({ path: 'test-results/mobile-settings.png', fullPage: true, animations: 'disabled' })
  await page.getByRole('button', { name: '删除密钥 公司线路', exact: true }).click()
  await page.getByRole('button', { name: '确认' }).click()
  await expect(page.getByText('密钥未配置', { exact: true })).toBeVisible()
})

test('断连显示过期状态，恢复无需刷新服务', async ({ page }) => {
  const state = await backend(page)
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  state.disconnected = true
  await expect(page.getByText('本地服务不可达')).toBeVisible({ timeout: 10000 })
  await expect(page.getByText('数据已过期').first()).toBeVisible()
  await expect(page.getByRole('button', { name: /官方模式/ })).toBeDisabled()
  await page.screenshot({ path: 'test-results/disconnected.png', fullPage: true, animations: 'disabled' })
  state.disconnected = false
  await expect(page.getByText('本地服务运行中')).toBeVisible({ timeout: 10000 })
  await expect(page.getByText('最近成功', { exact: true })).toBeVisible()
})

test('认证失效与不兼容版本均禁止写入', async ({ page }) => {
  const state = await backend(page)
  state.authenticated = false
  await page.goto('/')
  await expect(page.getByText('管理认证已失效', { exact: true })).toBeVisible()
  state.authenticated = true
  state.version = '2.0'
  await page.getByRole('button', { name: '刷新', exact: true }).click()
  await expect(page.getByText('接口版本不兼容', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '保存并应用' })).toHaveCount(0)
})

test('后台首次绑定及时锁定地址，保留未保存设置和供应商选择', async ({ page }) => {
  const state = await backend(page)
  state.providers[1].upstream_locked = false
  state.sessions.bindings = []
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  state.config.default_provider_id = 'company'
  await expect(page.getByRole('button', { name: /中转模式/ })).toHaveAttribute('aria-pressed', 'true', { timeout: 10000 })
  await page.getByRole('button', { name: /官方模式/ }).click()
  await page.getByRole('button', { name: '供应商设置' }).click()
  await page.getByLabel('连接超时（毫秒）').fill('24000')
  await page.getByRole('button', { name: '编辑 公司线路', exact: true }).click()
  await page.getByLabel('BaseUrl', { exact: false }).first().fill('https://unsaved.example.test/v1')
  state.providers[1].upstream_locked = true
  state.config.header_timeout_ms = 45000
  state.sessions.bindings.push({ id: 'new-binding', provider_id: 'company', created_at: '2026-11-03T12:40:00Z' })
  await expect(page.getByLabel('BaseUrl', { exact: false }).first()).toBeDisabled({ timeout: 10000 })
  await expect(page.getByLabel('BaseUrl', { exact: false }).first()).toHaveValue(state.providers[1].base_url)
  await expect(page.getByLabel('连接超时（毫秒）')).toHaveValue('24000')
  await expect(page.getByLabel('响应头超时（毫秒）')).toHaveValue('45000')
  await expect(page.getByText('未保存的地址已恢复为实际配置', { exact: false })).toBeVisible()
  await page.getByRole('button', { name: '关闭编辑', exact: true }).click()
  await page.getByRole('button', { name: '线路总览' }).click()
  await expect(page.getByRole('button', { name: /官方模式/ })).toHaveAttribute('aria-pressed', 'true')
  state.config.connect_timeout_ms = 19000
  await page.getByRole('button', { name: '保存并应用' }).click()
  await expect(page.getByText('已保存新会话默认供应商。已有会话保持原绑定。')).toBeVisible()
  expect(state.config.connect_timeout_ms).toBe(19000)
  expect(state.config.default_provider_id).toBe('official')
  await page.getByRole('button', { name: '供应商设置' }).click()
  await expect(page.getByLabel('连接超时（毫秒）')).toHaveValue('24000')
})

test('macOS 隔离能力限制与三视口显示', async ({ page }) => {
  const state = await backend(page)
  state.clients[0]!.state = 'probe_only'
  state.clients[0]!.message = '仅隔离启动检查，不启动交互代理'
  state.clients[1]!.state = 'isolation_unverified'
  state.clients[1]!.message = '桌面多实例隔离未验证，禁止启动'
  await page.goto('/')
  await page.getByRole('button', { name: '客户端与接入', exact: true }).click()
  await page.getByRole('button', { name: '独立测试', exact: true }).click()
  await expect(page.getByRole('button', { name: '隔离启动检查', exact: true })).toBeEnabled()
  await expect(page.getByRole('button', { name: '启动测试桌面版', exact: true })).toBeDisabled()
  await expect(page.getByRole('button', { name: '登录官方账号', exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: '恢复测试环境直连', exact: true })).toHaveCount(0)
  for (const [width, height] of [[1440, 900], [768, 1024], [390, 844]]) {
    await page.setViewportSize({ width, height })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await page.screenshot({ path: `test-results/macos-clients-${width}.png`, fullPage: true, animations: 'disabled' })
  }
})

test('专用客户端入口、模式切换与手机布局', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await backend(page)
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await page.getByRole('button', { name: '客户端与接入', exact: true }).click()
  await page.getByRole('button', { name: '独立测试', exact: true }).click()
  await expect(page.getByRole('button', { name: '启动测试 CLI', exact: true })).toBeEnabled()
  await expect(page.getByRole('button', { name: '启动测试桌面版', exact: true })).toBeEnabled()
  await page.screenshot({ path: 'test-results/desktop-clients.png', fullPage: true, animations: 'disabled' })
  await page.getByRole('button', { name: '恢复测试环境直连', exact: true }).first().click()
  await expect(page.getByText('下次启动：直连官方（不经过路由器）')).toBeVisible()
  await page.getByRole('button', { name: '启动测试 CLI', exact: true }).click()
  await expect(page.getByText('已启动', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '启动测试 CLI', exact: true })).toBeDisabled()
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: 'test-results/mobile-clients.png', fullPage: true, animations: 'disabled' })
})

test('模型弹窗搜索、键盘选择、失败恢复与一次保存供应商', async ({ page }) => {
  const state = await backend(page)
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await page.getByRole('button', { name: '供应商设置', exact: true }).click()
  await page.getByRole('button', { name: '添加供应商', exact: true }).click()
  await page.locator('#new-provider-name').fill('备用中转')
  await page.locator('#new-provider-url').fill('https://new.example.test/api')
  await page.locator('#new-provider-key').fill('synthetic-new-key')
  await page.getByRole('button', { name: '拉取模型', exact: true }).last().click()
  const dialog = page.getByRole('dialog', { name: '选择默认模型' })
  await expect(dialog).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'gpt-test-b 选择' })).toBeVisible()
  await page.screenshot({ path: 'test-results/desktop-models.png', animations: 'disabled' })
  await dialog.getByLabel('搜索模型').fill('missing-model')
  await expect(dialog.getByText('没有匹配的模型，请换个关键词。')).toBeVisible()
  await dialog.getByLabel('搜索模型').fill('test-b')
  await expect(dialog.getByRole('button', { name: 'gpt-test-a 选择' })).toHaveCount(0)
  await dialog.getByRole('button', { name: 'gpt-test-b 选择' }).focus()
  await page.keyboard.press('Enter')
  await expect(dialog).not.toBeVisible()
  await expect(page.locator('#new-provider-model')).toHaveValue('gpt-test-b')
  state.failProviderSave = true
  await page.getByRole('button', { name: '保存供应商' }).last().click()
  await expect(page.getByText('供应商未保存；原配置仍有效')).toBeVisible()
  expect(state.providers).toHaveLength(2)
  await expect(page.locator('#new-provider-name')).toHaveValue('备用中转')
  await expect(page.locator('#new-provider-key')).toHaveValue('synthetic-new-key')
  state.failProviderSave = false
  await page.locator('#new-provider-key').fill('synthetic-new-key')
  await page.getByRole('button', { name: '保存供应商' }).last().click()
  await expect(page.getByText('供应商已添加。可在线路总览选择中转模式并应用。')).toBeVisible()
  expect(state.providers[2]).toMatchObject({ name: '备用中转', model: 'gpt-test-b', key_configured: true })
  expect(JSON.stringify(state.providers)).not.toContain('synthetic-new-key')
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('button', { name: '编辑 公司线路', exact: true }).click()
  await page.getByRole('button', { name: '拉取模型', exact: true }).first().click()
  await expect(dialog.getByRole('button', { name: 'gpt-test-a 选择' })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: 'test-results/mobile-models.png', animations: 'disabled' })
  await page.keyboard.press('Escape')
  await expect(dialog).not.toBeVisible()
  await expect(page.getByRole('button', { name: '拉取模型', exact: true }).first()).toBeFocused()
  state.failModels = true
  await page.getByRole('button', { name: '拉取模型', exact: true }).first().click()
  await expect(dialog.getByText('该地址不提供模型列表，可手动填写模型后保存')).toBeVisible()
  await page.screenshot({ path: 'test-results/mobile-model-error.png', animations: 'disabled' })
  state.failModels = false
  await dialog.getByRole('button', { name: '重新拉取' }).click()
  await expect(dialog.getByRole('button', { name: 'gpt-test-a 选择' })).toBeVisible()
  await dialog.getByRole('button', { name: '关闭', exact: true }).click()
  await page.getByRole('button', { name: '关闭编辑', exact: true }).click()
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.evaluate(() => scrollTo(0, 0))
  await page.screenshot({ path: 'test-results/desktop-providers.png', fullPage: true, animations: 'disabled' })
})

test('只有两种模式，未配置中转不能应用且网站图标可访问', async ({ page }) => {
  const state = await backend(page)
  state.providers = state.providers.filter(p => p.kind === 'official')
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  const modes = page.getByRole('group', { name: '新会话模式' })
  await expect(modes.getByRole('button')).toHaveCount(2)
  await modes.getByRole('button', { name: /中转模式/ }).click()
  await expect(page.getByRole('button', { name: '保存并应用' })).toBeDisabled()
  await page.getByRole('button', { name: '添加供应商', exact: true }).click()
  await expect(page.getByRole('heading', { name: '添加供应商', exact: true })).toBeVisible()
  const href = await page.locator('link[rel="icon"]').getAttribute('href')
  expect(href).toBe('/favicon.svg')
  expect((await page.request.get(href!)).status()).toBe(200)
})

test('日常接入预览取消、冲突拒绝、应用及恢复，测试入口单独保留', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  const state = await backend(page)
  state.daily.config_path = 'C:/Users/Example/AppData/Local/Workspaces/Office Tasks/Codex Home With Long Folder Name/config.toml'
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await page.getByRole('button', { name: '客户端与接入', exact: true }).click()
  await expect(page.getByRole('heading', { name: '接入原来的 Codex' })).toBeVisible()
  await expect(page.getByRole('button', { name: '启动测试 CLI', exact: true })).toHaveCount(0)
  await page.screenshot({ path: 'test-results/desktop-daily.png', fullPage: true, animations: 'disabled' })
  await page.getByRole('button', { name: '接入 CodexRouter', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: /确认(接入日常 Codex|恢复接入前配置)/ })
  await expect(dialog).toBeVisible()
  await expect(dialog.getByText('openai_base_url', { exact: true })).toBeVisible()
  await page.screenshot({ path: 'test-results/desktop-daily-preview.png', animations: 'disabled' })
  await dialog.getByRole('button', { name: '取消', exact: true }).click()
  expect(state.daily.state).toBe('direct')
  await page.getByRole('button', { name: '接入 CodexRouter', exact: true }).click()
  state.dailyFail = true
  await dialog.getByRole('button', { name: /确认(接入|恢复)/, exact: true }).click()
  await expect(dialog.getByText(/预览后配置发生变化/)).toBeVisible()
  await expect(dialog.getByRole('button', { name: /确认(接入|恢复)/ })).toBeDisabled()
  expect(state.daily.state).toBe('direct')
  await dialog.getByRole('button', { name: '取消', exact: true }).click()
  state.dailyFail = false
  await page.getByRole('button', { name: '接入 CodexRouter', exact: true }).click()
  await dialog.getByRole('button', { name: /确认(接入|恢复)/, exact: true }).click()
  await expect(page.getByText('配置已接入', { exact: true })).toBeVisible()
  expect(state.daily.has_backup).toBe(true)
  await page.setViewportSize({ width: 390, height: 844 })
  await page.evaluate(() => scrollTo(0, 0))
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: 'test-results/mobile-daily.png', fullPage: true, animations: 'disabled' })
  await page.getByRole('button', { name: '恢复接入前配置', exact: true }).click()
  await expect(dialog).toHaveAttribute('aria-label', '确认恢复接入前配置')
  await expect(dialog.getByRole('button', { name: '确认恢复', exact: true })).toBeVisible()
  await expect(dialog.getByText('移除接入字段')).toBeVisible()
  await page.screenshot({ path: 'test-results/mobile-daily-preview.png', animations: 'disabled' })
  await dialog.getByRole('button', { name: /确认(接入|恢复)/, exact: true }).click()
  await expect(page.getByText('尚未接入', { exact: true })).toBeVisible()
  await expect(page.getByText(/已恢复接入前的连接配置/)).toBeVisible()
  await page.getByRole('button', { name: '独立测试', exact: true }).click()
  await expect(page.getByRole('button', { name: '启动测试 CLI', exact: true })).toBeVisible()
})

test('主页实际请求证据与配置分离，刷新不改变会话', async ({ page }) => {
  const state = await backend(page)
  await page.goto('/')
  await expect(page.getByRole('heading', { name: '尚未收到客户端请求', exact: true })).toBeVisible()
  state.activity = { received: 1, degraded: false, items: [syntheticRequest({ id: 1, time: new Date().toISOString(), entry: 'POST /responses', thread_id: 'synthetic-received-session', provider_id: 'company', phase: 'finished', result: 'HTTP_200', status: 200, elapsed_ms: 150 })] }
  await page.getByRole('button', { name: '刷新请求', exact: true }).click()
  await expect(page.getByRole('heading', { name: '已收到客户端请求', exact: true })).toBeVisible()
  await page.getByRole('button', { name: '查看请求明细', exact: true }).click()
  await expect(page.getByText('synthetic-received-session', { exact: true })).toBeVisible()
  expect(state.config.default_provider_id).toBe('official')
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.screenshot({ path: 'test-results/desktop-activity.png', fullPage: true, animations: 'disabled' })
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: 'test-results/mobile-activity.png', fullPage: true, animations: 'disabled' })
})


test('请求表格后端分页筛选、失败重试、清理确认与保留策略', async ({ page }) => {
  const state = await backend(page)
  state.activity = { received: 46, degraded: false, items: Array.from({ length: 46 }, (_, index) => syntheticRequest({ id: 46 - index, time: '2026-11-05T10:00:00Z', entry: 'POST /responses', thread_id: `synthetic-long-session-${index}-${'a'.repeat(80)}`, provider_id: 'company', phase: index === 0 ? 'received' : 'finished', result: index === 0 ? '处理中' : index % 2 ? 'HTTP_200' : 'STREAM_INTERRUPTED', status: index === 0 ? 0 : 200, elapsed_ms: index * 123 })) }
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await navigate(page, '请求明细')
  const region = page.getByRole('region', { name: '请求明细，可横向滚动' })
  await expect(region.locator('tbody tr')).toHaveCount(20)
  await page.getByRole('button', { name: '下一页', exact: true }).click()
  await expect(page.getByRole('button', { name: '第 2 页', exact: true })).toHaveAttribute('aria-current', 'page')
  await expect(region.locator('tbody tr')).toHaveCount(3)
  await expect(page.getByRole('button', { name: '下一页', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: '上一页', exact: true }).click()
  await expect(region.locator('tbody tr')).toHaveCount(20)
  await page.getByRole('tab', { name: '错误请求', exact: true }).click()
  await expect(page.getByRole('button', { name: '第 1 页', exact: true })).toHaveAttribute('aria-current', 'page')
  await expect(region.locator('tr[data-outcome="success"]')).toHaveCount(0)
  state.failHistory = true
  await page.getByRole('button', { name: '刷新记录', exact: true }).click()
  await expect(page.getByText(/历史暂不可用，请重试/)).toBeVisible()
  state.failHistory = false
  await page.getByRole('button', { name: '重试读取', exact: true }).click()
  await expect(page.getByText(/历史暂不可用，请重试/)).toHaveCount(0)
  await navigate(page, '历史管理')
  await page.getByLabel('保留天数').fill('14')
  await page.waitForTimeout(5200)
  await expect(page.getByLabel('保留天数')).toHaveValue('14')
  await page.getByRole('button', { name: '保存保留策略' }).click()
  await expect(page.getByText(/保留策略已保存并执行/)).toBeVisible()
  expect(state.historyDays).toBe(14)
  await page.getByRole('button', { name: '清除全部请求历史' }).click()
  const dialog = page.getByRole('dialog', { name: '确认清除请求历史' })
  await expect(dialog).toBeVisible()
  await dialog.getByRole('button', { name: '取消', exact: true }).click()
  expect(state.activity.items).toHaveLength(46)
  await page.getByRole('button', { name: '清除全部请求历史' }).click()
  state.failClear = true
  await dialog.getByRole('button', { name: '确认清除', exact: true }).click()
  await expect(dialog.getByText('清理未完成，请重试')).toBeVisible()
  expect(state.activity.items).toHaveLength(46)
  state.failClear = false
  await dialog.getByRole('button', { name: '确认清除', exact: true }).click()
  await expect(page.getByText(/已清除 45 条/)).toBeVisible()
  expect(state.sessions.bindings).toHaveLength(1)
  expect(state.sessions.pending).toHaveLength(1)
  expect(state.activity.items).toHaveLength(1)
  await navigate(page, '请求明细')
  await page.getByRole('tab', { name: '处理中', exact: true }).click()
  await expect(region.locator('tbody tr')).toHaveCount(1)
  await page.getByLabel('每页', { exact: false }).first().selectOption('50')
  await expect(page.getByRole('button', { name: '下一页', exact: true })).toBeDisabled()
})

test('会话独立分页、供应商编辑草稿与各页面多视口截图', async ({ page }) => {
  const state = await backend(page)
  state.sessions.bindings = Array.from({ length: 41 }, (_, i) => ({ id: `synthetic-bound-${i}-${'x'.repeat(90)}`, provider_id: 'official', created_at: '2026-11-05T10:00:00Z' }))
  state.activity = { received: 1, degraded: false, items: [syntheticRequest({ id: 1, time: '2026-11-05T10:00:00Z', entry: 'POST /responses', thread_id: 'synthetic-session-table', provider_id: 'company', phase: 'finished', result: 'HTTP_200', status: 200, elapsed_ms: 1234 })] }
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await navigate(page, '已绑定会话')
  const bound = page.getByRole('region', { name: '已绑定会话表格，可横向滚动' })
  await expect(bound.locator('tbody tr')).toHaveCount(20)
  await page.getByRole('button', { name: '下一页', exact: true }).last().click()
  await expect(bound.locator('tbody tr')).toHaveCount(20)
  await page.getByRole('button', { name: '下一页', exact: true }).last().click()
  await expect(bound.locator('tbody tr')).toHaveCount(1)
  await page.getByRole('button', { name: '刷新已绑定', exact: true }).click()
  await expect(bound.locator('tbody tr')).toHaveCount(20)
  for (const viewport of [{ width: 1440, height: 900 }, { width: 2552, height: 1274 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport)
    for (const label of ['线路总览', '请求明细', '历史管理', '客户端与接入', '供应商设置', '待登记会话', '已绑定会话', '近期诊断']) {
      await navigate(page, label)
      await expect(page.getByRole('heading', { name: label, exact: true })).toBeVisible()
      if (label === '请求明细') await expect(page.getByText('synthetic-session-table', { exact: true })).toBeVisible()
      await page.evaluate(() => scrollTo(0, 0))
      const overflow = await page.evaluate(() => ({ width: innerWidth, scroll: document.documentElement.scrollWidth, elements: Array.from(document.querySelectorAll('main, main > *, .section-heading, .table-scroll, .pagination')).filter(el => el.getBoundingClientRect().right > innerWidth).map(el => ({ tag: el.tagName, cls: el.className, right: el.getBoundingClientRect().right, width: el.getBoundingClientRect().width })) }))
      expect(overflow.scroll, `${viewport.width} ${label} ${JSON.stringify(overflow)}`).toBeLessThanOrEqual(viewport.width)
      await page.screenshot({ path: `test-results/final-${viewport.width}-${label}.png`, fullPage: true, animations: 'disabled' })
    }
  }
})


test('会话索引失败只降级历史展示，保留其他管理功能及草稿', async ({ page }) => {
  const state = await backend(page)
  await page.goto('/')
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await page.getByRole('button', { name: '供应商设置', exact: true }).click()
  await page.getByLabel('连接超时（毫秒）').fill('22000')
  state.failSessions = true
  await expect(page.getByText(/会话索引暂不可用/)).toBeVisible({ timeout: 10000 })
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await expect(page.getByRole('button', { name: '保存超时与并发' })).toBeEnabled()
  await expect(page.getByLabel('连接超时（毫秒）')).toHaveValue('22000')
  state.failSessions = false
  await expect(page.getByText(/会话索引暂不可用/)).toHaveCount(0, { timeout: 10000 })
  await expect(page.getByLabel('连接超时（毫秒）')).toHaveValue('22000')
})


test('空用量页可导航，辅助请求不冒充生成指标，缺失原因可见', async ({ page }) => {
  const state = await backend(page)
  const base = syntheticRequest({ id: 1, time: '2026-11-06T01:30:00Z', entry: 'GET /models', thread_id: '', provider_id: 'official', phase: 'finished', result: 'HTTP_200', outcome: 'unconfirmed', status: 200, elapsed_ms: 4040 })
  const emptyMetrics = { ...base.metrics, known_fields: [] }
  state.activity = { received: 5, degraded: false, items: [
    { ...base, request_type: 'auxiliary', metrics: emptyMetrics, observation: { terminal_result: 'unknown', transport_result: 'EOF', metrics_status: 'not_applicable' } },
    { ...base, id: 2, entry: 'POST /responses', request_type: 'unknown', metrics: emptyMetrics, observation: { terminal_result: 'unknown', transport_result: '', metrics_status: 'legacy' } },
    { ...base, id: 3, entry: 'POST /responses', request_type: 'stream', metrics: emptyMetrics, observation: { terminal_result: 'unknown', transport_result: 'EOF', metrics_status: 'unavailable' } },
    { ...base, id: 4, entry: 'POST /responses', request_type: 'stream', metrics: emptyMetrics, observation: { terminal_result: 'unknown', transport_result: 'EOF', metrics_status: 'not_reported' } },
    { ...base, id: 5, entry: 'POST /responses', request_type: 'stream', metrics: { ...emptyMetrics, input_tokens: 123, known_fields: ['input_tokens'] }, observation: { terminal_result: 'RESPONSE_FAILED', transport_result: 'REQUEST_CANCELED', metrics_status: 'partial' }, outcome: 'error', result: 'RESPONSE_FAILED' },
  ] }
  await page.goto('/')
  await navigate(page, '请求明细')
  await expect(page.getByText('暂无确认成功的生成请求', { exact: true })).toBeVisible()
  for (const width of [1440, 2552, 390]) {
    await page.setViewportSize({ width, height: width === 390 ? 844 : width === 2552 ? 1274 : 900 })
    await page.screenshot({ path: `test-results/final-${width}-requests-empty.png`, animations: 'disabled' })
  }
  await page.getByRole('button', { name: '查看待确认请求', exact: true }).click()
  const region = page.getByRole('region', { name: '请求明细，可横向滚动' })
  const auxiliary = region.locator('tbody tr').first()
  await expect(auxiliary.getByText('不适用', { exact: true })).toHaveCount(3)
  await expect(auxiliary.getByText('4.04s', { exact: true })).toBeVisible()
  await expect(region.getByText('旧记录缺少指标', { exact: true })).toBeVisible()
  await expect(region.getByText('解析未完成', { exact: true })).toBeVisible()
  await expect(region.getByText('上游未返回指标', { exact: true })).toBeVisible()
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.screenshot({ path: 'test-results/final-1440-requests-metric-reasons.png', animations: 'disabled' })
  await page.getByRole('tab', { name: '错误请求', exact: true }).click()
  await expect(region.getByText('部分指标未返回', { exact: true })).toBeVisible()
  await region.getByRole('button', { name: '查看请求 5 用量明细', exact: true }).click()
  const detail = page.getByRole('dialog', { name: '请求用量明细', exact: true })
  await expect(detail.getByText('123', { exact: true })).toBeVisible()
  await expect(detail.getByText('客户端关闭连接', { exact: true })).toBeVisible()
  await page.screenshot({ path: 'test-results/final-1440-partial-error-detail.png', animations: 'disabled' })
})


test('两级菜单键盘操作与会话子页保留状态、登记和复制', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
  const state = await backend(page)
  state.sessions.pending = Array.from({ length: 61 }, (_, i) => ({ id: `pending-${i}-${'p'.repeat(90)}`, first_seen: '2026-11-06T01:02:03Z', reason: 'unknown_origin' }))
  state.sessions.bindings = Array.from({ length: 61 }, (_, i) => ({ id: `bound-${i}-${'b'.repeat(90)}`, provider_id: 'company', created_at: '2026-11-06T01:02:03Z' }))
  let inferenceCalls = 0
  page.on('request', request => { if (new URL(request.url()).pathname === '/responses') inferenceCalls++ })
  await page.goto('/')
  const group = page.getByRole('button', { name: '会话管理', exact: true })
  await expect(group).toHaveAttribute('aria-expanded', 'false')
  await group.focus(); await page.keyboard.press('Enter')
  await expect(group).toHaveAttribute('aria-expanded', 'true')
  await expect(page.getByRole('heading', { name: '线路总览', exact: true })).toBeVisible()
  await page.getByRole('button', { name: '待登记会话', exact: true }).focus(); await page.keyboard.press('Enter')
  await expect(page.getByRole('button', { name: '待登记会话', exact: true })).toHaveAttribute('aria-current', 'page')
  await page.getByLabel('待登记会话每页', { exact: true }).selectOption('50')
  await page.getByRole('button', { name: '下一页', exact: true }).click()
  const pendingRegion = page.getByRole('region', { name: '待登记会话表格，可横向滚动' })
  await expect(pendingRegion.locator('tbody tr')).toHaveCount(11)
  const id = state.sessions.pending[50]!.id
  await pendingRegion.getByRole('button', { name: id, exact: true }).focus(); await page.keyboard.press('Enter')
  const idDialog = page.getByRole('dialog', { name: '完整会话 ID', exact: true })
  await expect(idDialog.getByText(id, { exact: true })).toBeVisible()
  await page.screenshot({ path: 'test-results/final-1440-full-session-id.png', animations: 'disabled' })
  await idDialog.getByRole('button', { name: '复制完整 ID', exact: true }).click()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(id)
  await idDialog.getByRole('button', { name: '关闭', exact: true }).click()
  const choice = page.getByLabel(`会话 ${id} 首次供应商`, { exact: true })
  await choice.selectOption('official')
  await page.getByRole('button', { name: `复制 ${id}`, exact: true }).click()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(id)
  await navigate(page, '已绑定会话')
  await page.getByLabel('已绑定会话每页', { exact: true }).selectOption('50')
  await page.getByRole('button', { name: '下一页', exact: true }).click()
  await expect(page.locator('.session-view .list-pagination')).toContainText('第 2 页')
  await navigate(page, '待登记会话')
  await expect(page.getByLabel('待登记会话每页', { exact: true })).toHaveValue('50')
  await expect(page.locator('.session-view .list-pagination')).toContainText('第 2 页')
  await expect(choice).toHaveValue('official')
  await page.waitForTimeout(5100)
  await expect(choice).toHaveValue('official')
  await pendingRegion.locator('tbody tr').first().getByRole('button', { name: '确认供应商', exact: true }).click()
  await expect(page.locator('.session-view .list-pagination')).toContainText('共 60 条')
  await navigate(page, '已绑定会话')
  await expect(page.locator('.session-view .list-pagination')).toContainText('共 62 条')
  expect(state.sessions.bindings.find(item => item.id === id)?.provider_id).toBe('official')
  expect(inferenceCalls).toBe(0)
  await group.click()
  await expect(group).toHaveAttribute('aria-expanded', 'false')
  await group.click()
  await expect(page.getByRole('button', { name: '已绑定会话', exact: true })).toHaveAttribute('aria-current', 'page')
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('button', { name: '请求管理', exact: true }).click()
  await page.getByRole('navigation', { name: '二级导航' }).getByRole('button', { name: '历史管理', exact: true }).click()
  await expect(page.getByRole('heading', { name: '历史管理', exact: true })).toBeVisible()
  await page.getByRole('button', { name: '会话管理', exact: true }).click()
  await expect(page.getByRole('heading', { name: '已绑定会话', exact: true })).toBeVisible()
  await page.getByRole('navigation', { name: '二级导航' }).getByRole('button', { name: '待登记会话', exact: true }).click()
  await expect(page.getByRole('heading', { name: '待登记会话', exact: true })).toBeVisible()
  await page.locator('.session-id-value').first().click()
  await expect(idDialog).toBeVisible()
  await idDialog.getByRole('button', { name: '复制完整 ID', exact: true }).click()
  await expect(idDialog.getByText('会话 ID 已复制。', { exact: true })).toBeVisible()
  await page.screenshot({ path: 'test-results/final-390-full-session-id.png', animations: 'disabled' })
  await page.keyboard.press('Escape')
  await expect(idDialog).not.toBeVisible()
})

test('六十条数据逐页验收滚动、紧凑行高、长字段与三种视口', async ({ page }) => {
  // 三视口逐页截图在整套验收负载下需要额外预算，断言保持完整。
  test.setTimeout(180000)
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
  const state = await backend(page)
  state.activity = { received: 61, degraded: false, items: Array.from({ length: 61 }, (_, i) => syntheticRequest({ id: i + 1, time: '2026-11-06T01:02:03.123Z', entry: 'POST /responses', thread_id: `compact-session-${i}-${'s'.repeat(90)}`, provider_id: 'company', phase: 'finished', outcome: 'success', result: 'RESPONSE_COMPLETED', status: 200, elapsed_ms: 310000, model: i % 5 ? 'gpt-6.1-sol' : 'model-' + 'm'.repeat(110) })) }
  state.activity.items.forEach((item, i) => { item.metrics.first_token_ms = [6490, 10000, 30000, 60000][i % 4]!; item.metrics.protocol_completed_ms = [68000, 60000, 180000, 300000][i % 4]! })
  state.sessions.pending = Array.from({ length: 61 }, (_, i) => ({ id: `pending-compact-${i}-${'p'.repeat(90)}`, first_seen: '2026-11-06T01:02:03Z', reason: 'unknown_origin' }))
  state.sessions.bindings = Array.from({ length: 61 }, (_, i) => ({ id: `bound-compact-${i}-${'b'.repeat(90)}`, provider_id: 'company', created_at: '2026-11-06T01:02:03Z' }))
  state.diagnostics.items = Array.from({ length: 61 }, (_, i) => ({ time: '2026-11-06T01:02:03Z', code: i % 2 ? 'RESPONSE_COMPLETED' : 'REQUEST_CANCELED', provider_id: 'company' }))
  const company = state.providers[1]!
  state.providers.push(...Array.from({ length: 5 }, (_, i) => ({ ...company, id: `p-extra-${i}`, name: `中转供应商 ${i}`, base_url: `https://example.test/upstream/${i}` })))
  await page.goto('/')
  const measurements: unknown[] = []
  for (const viewport of [{ width: 1440, height: 900 }, { width: 2552, height: 1274 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport)
    for (const label of ['请求明细', '待登记会话', '已绑定会话', '近期诊断']) {
      await navigate(page, label)
      if (label === '请求明细') await page.getByLabel('每页', { exact: false }).first().selectOption('50')
      else if (label !== '近期诊断') await page.getByLabel(`${label}每页`, { exact: true }).selectOption('50')
      const region = page.locator('.data-viewport')
      await expect(region.locator('tbody tr')).toHaveCount(label === '近期诊断' ? 61 : 50)
      await region.evaluate(el => { el.scrollTop = 0; el.scrollLeft = 0 })
      const header = await region.locator('thead').boundingBox()
      const heading = await page.locator('.page-heading').boundingBox()
      const footer = await page.locator('.list-pagination').boundingBox()
      const metrics = await region.evaluate(el => {
        const viewport = el.getBoundingClientRect()
        const rows = Array.from(el.querySelectorAll('tbody tr')).map(row => row.getBoundingClientRect())
        const head = el.querySelector('thead')!.getBoundingClientRect()
        return { rowHeights: rows.map(row => row.height), visibleRows: rows.filter(row => row.top >= head.bottom && row.bottom <= viewport.bottom).length, clientHeight: el.clientHeight, scrollHeight: el.scrollHeight }
      })
      if (label === '请求明细') {
        expect(await region.locator('thead th').allTextContents()).toEqual(['供应商', '会话 ID', '模型', '请求', 'Token', '延迟', '结果', '时间'])
        expect(Math.min(...metrics.rowHeights)).toBeGreaterThanOrEqual(60)
        expect(Math.max(...metrics.rowHeights)).toBeLessThanOrEqual(64)
        expect(metrics.visibleRows).toBeGreaterThanOrEqual(viewport.width === 390 ? 4 : viewport.width === 1440 ? 8 : 14)
        const fullId = state.activity.items[0]!.thread_id
        await page.getByRole('button', { name: `复制 ${fullId}`, exact: true }).click()
        expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(fullId)
        expect(await region.locator('tbody td').nth(1).locator('.truncate').evaluate(el => el.scrollWidth > el.clientWidth)).toBe(true)
      }
      measurements.push({ viewport, page: label, ...metrics })
      await page.screenshot({ path: `test-results/final-${viewport.width}-compact-${label}-before.png`, animations: 'disabled' })
      await region.evaluate(el => { el.scrollTop = el.scrollHeight })
      expect(await region.evaluate(el => el.scrollTop)).toBeGreaterThan(0)
      expect(await page.locator('main').evaluate(el => el.scrollTop)).toBe(0)
      expect(await page.locator('.page-content').evaluate(el => el.scrollTop)).toBe(0)
      expect(await region.locator('thead').boundingBox()).toEqual(header)
      expect(await page.locator('.page-heading').boundingBox()).toEqual(heading)
      expect(await page.locator('.list-pagination').boundingBox()).toEqual(footer)
      await page.screenshot({ path: `test-results/final-${viewport.width}-compact-${label}-after.png`, animations: 'disabled' })
      if (viewport.width === 390) {
        await region.evaluate(el => { el.scrollLeft = el.scrollWidth })
        await page.screenshot({ path: `test-results/final-390-compact-${label}-horizontal.png`, animations: 'disabled' })
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth && document.documentElement.scrollHeight <= innerHeight)).toBe(true)
    }
    for (const label of ['线路总览', '历史管理', '客户端与接入', '供应商设置']) {
      await navigate(page, label)
      const heading = await page.locator('.page-heading').boundingBox()
      const content = page.locator('.page-content')
      await content.evaluate(el => { el.scrollTop = 0 })
      await page.screenshot({ path: `test-results/final-${viewport.width}-content-${label}-before.png`, animations: 'disabled' })
      await content.evaluate(el => { el.scrollTop = el.scrollHeight })
      expect(await page.locator('.page-heading').boundingBox()).toEqual(heading)
      expect(await page.locator('main').evaluate(el => el.scrollTop)).toBe(0)
      expect(await page.evaluate(() => window.scrollY)).toBe(0)
      if (label === '供应商设置') await expect(page.getByRole('button', { name: '保存超时与并发', exact: true })).toBeInViewport()
      await page.screenshot({ path: `test-results/final-${viewport.width}-content-${label}-after.png`, animations: 'disabled' })
    }
  }
  writeFileSync('test-results/final-layout-metrics.json', JSON.stringify(measurements, null, 2))
})


test('旧页面接收新引导后恢复并保留未保存设置', async ({ page }) => {
 const state = await backend(page)
 await page.goto('/')
 await expect(page.getByText('本地服务运行中')).toBeVisible()
 await page.getByRole('button', { name: '供应商设置', exact: true }).click()
 await page.getByLabel('连接超时（毫秒）').fill('17000')
 state.authenticated = false
 await page.getByRole('button', { name: '刷新', exact: true }).click()
 await expect(page.getByText('管理认证已失效', { exact: true })).toBeVisible()
 state.authenticated = true
 await page.evaluate(() => { location.hash = 'token=synthetic-new-bootstrap' })
 await expect(page.getByText('本地服务运行中')).toBeVisible()
 expect(new URL(page.url()).hash).toBe('')
 await expect(page.getByLabel('连接超时（毫秒）')).toHaveValue('17000')
 await expect(page.getByRole('button', { name: '保存超时与并发' })).toBeEnabled()
})

test('连续新引导忽略迟到旧认证失败并恢复轮询', async ({ page }) => {
 await backend(page)
 await page.goto('/')
 await expect(page.getByText('本地服务运行中')).toBeVisible()
 let release: () => void = () => {}
 let received: () => void = () => {}
 const arrived = new Promise<void>(resolve => { received = resolve })
 const blocked = new Promise<void>(resolve => { release = resolve })
 await page.route('**/api/v1/auth/exchange', async route => {
  const value = route.request().postDataJSON() as { token: string }
  if (value.token === 'synthetic-first') {
   received(); await blocked
   await route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { code: 'AUTH_REQUIRED', message: 'old auth' } }) }).catch(() => {})
  } else await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ csrf_token: 'synthetic-new' }) })
 })
 await page.evaluate(() => { location.hash = 'token=synthetic-first' })
 await arrived
 await page.evaluate(() => { location.hash = 'token=synthetic-second' })
 await expect(page.getByText('本地服务运行中')).toBeVisible()
 release()
 await page.getByRole('button', { name: '刷新', exact: true }).click()
 await expect(page.getByText('本地服务运行中')).toBeVisible()
 expect(new URL(page.url()).hash).toBe('')
})
