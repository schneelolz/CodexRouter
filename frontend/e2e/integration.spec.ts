import { createServer } from 'node:http'
import { test, expect } from '@playwright/test'

test('生产静态目录与真实 Go 管理 API 联调', async ({ page, context }) => {
  const token = process.env.CODEXROUTER_E2E_TOKEN
  if (!token) throw new Error('集成测试缺少专用引导令牌')
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.goto(`/#token=${token}`)
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await expect(page.getByRole('button', { name: '刷新', exact: true })).toBeEnabled()
  expect(new URL(page.url()).hash).toBe('')
  await expect(page.getByText('未验证', { exact: true })).toHaveCount(2)
  const cookie = (await context.cookies()).find(x => x.name === 'codexrouter_session')
  expect(cookie?.httpOnly).toBe(true)
  expect(cookie?.sameSite).toBe('Strict')
  expect(cookie?.path).toBe('/api/v1/')
  const renewed = await page.evaluate(async () => {
    const session = await (await fetch('/api/v1/auth/session')).json()
    return (await fetch('/api/v1/auth/renew', { method: 'POST', headers: { 'X-CSRF-Token': session.csrf_token } })).status
  })
  expect(renewed).toBe(200)

  await page.getByRole('button', { name: '客户端与接入', exact: true }).click()
  await page.getByRole('button', { name: '接入 CodexRouter', exact: true }).click()
  const dailyDialog = page.getByRole('dialog', { name: /确认(接入日常 Codex|恢复接入前配置)/ })
  await expect(dailyDialog).toBeVisible()
  await dailyDialog.getByRole('button', { name: /确认(接入|恢复)/, exact: true }).click()
  await expect(page.getByText('配置已接入', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: '恢复接入前配置', exact: true }).click()
  await dailyDialog.getByRole('button', { name: /确认(接入|恢复)/, exact: true }).click()
  await expect(page.getByText('尚未接入', { exact: true })).toBeVisible()
  await page.screenshot({ path: 'test-results-integration/real-backend-daily.png', fullPage: true, animations: 'disabled' })

  await page.getByRole('button', { name: '供应商设置' }).click()
  await page.getByLabel('连接超时（毫秒）').fill('11000')
  await page.getByRole('button', { name: '保存超时与并发' }).click()
  await expect(page.getByText('超时与并发设置已保存。')).toBeVisible()
  await expect(page.getByRole('button', { name: '保存超时与并发' })).toBeEnabled()

  const upstream = createServer((request, response) => {
    if (request.url === '/api/models' && request.headers.authorization === 'Bearer synthetic-integration-company-key') {
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ data: [{ id: 'synthetic-model-a' }, { id: 'synthetic-model-b' }] }))
    } else if (request.url === '/api/responses' && request.headers.authorization === 'Bearer synthetic-integration-company-key') {
      response.setHeader('Content-Type', 'text/event-stream')
      let body = ''
      request.on('data', chunk => { body += String(chunk) })
      request.on('end', () => {
        const failed = JSON.parse(body).input[0].content === 'synthetic-failure'
        response.write('data: {"type":"response.created"}\n\n')
        if (!failed) response.write('data: {"type":"response.output_text.delta","delta":"合成输出"}\n\n')
        response.end(`data: ${JSON.stringify({ type: failed ? 'response.failed' : 'response.completed', response: { status: failed ? 'failed' : 'completed', model: 'synthetic-model-b', output: failed ? [] : [{ type: 'message' }], usage: { input_tokens: 144746, output_tokens: failed ? 0 : 630, total_tokens: failed ? 144746 : 145376, input_tokens_details: { cached_tokens: 144400 } } } })}\n\n`)
      })
    } else { response.statusCode = 401; response.end('{}') }
  })
  await new Promise<void>(resolve => upstream.listen(0, '127.0.0.1', resolve))
  const address = upstream.address()
  if (!address || typeof address === 'string') throw new Error('合成模型服务未启动')
  try {
    await page.getByRole('button', { name: '编辑 公司线路', exact: true }).click()
    await page.locator('#provider-company-url').fill(`http://127.0.0.1:${address.port}/api`)
    await page.locator('#provider-company-key').fill('synthetic-integration-company-key')
    await page.getByRole('button', { name: '拉取模型' }).first().click()
    await expect(page.getByRole('dialog', { name: '选择默认模型' })).toBeVisible()
    await page.getByRole('button', { name: 'synthetic-model-b 选择' }).click()
    await expect(page.locator('#provider-company-model')).toHaveValue('synthetic-model-b')
    await page.getByRole('button', { name: '保存供应商' }).first().click()
    await expect(page.getByText('供应商已保存。默认模型对下一条请求生效。')).toBeVisible()
    await page.getByRole('button', { name: '编辑 公司线路', exact: true }).click()
    await expect(page.locator('#provider-company-key')).toHaveValue('')
    // 留空再次查询，实际由Go从专用凭据后端取出原地址的密钥。
    await page.getByRole('button', { name: '拉取模型' }).first().click()
    await expect(page.getByRole('button', { name: 'synthetic-model-a 选择' })).toBeVisible()
    await page.getByRole('button', { name: '关闭', exact: true }).click()
    await page.getByRole('button', { name: '关闭编辑', exact: true }).click()
    const session = await page.evaluate(async () => (await (await fetch('/api/v1/auth/session')).json()).csrf_token as string)
    await page.evaluate(async csrf => {
      const config = await (await fetch('/api/v1/config')).json()
      const { version: _, ...input } = config
      const result = await fetch('/api/v1/config', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ ...input, default_provider_id: 'company' }) })
      if (!result.ok) throw new Error('合成默认供应商设置失败')
    }, session)
    for (const kind of ['success', 'failure']) {
      const id = `synthetic-browser-${kind}`
      const result = await context.request.post('/responses', { headers: { 'Thread-Id': id, 'Session-Id': id, 'X-Codex-Turn-Metadata': JSON.stringify({ thread_id: id, session_id: id, history_initialization: 'new' }) }, data: { stream: true, input: [{ role: 'user', content: `synthetic-${kind}` }] } })
      expect(result.status()).toBe(200)
      expect(await result.text()).toContain(kind === 'success' ? 'response.completed' : 'response.failed')
    }
    await page.getByRole('button', { name: '请求管理', exact: true }).click()
    await page.getByRole('button', { name: '请求明细', exact: true }).click()
    await expect(page.getByText('synthetic-browser-success', { exact: true })).toBeVisible()
    await expect(page.getByText('synthetic-browser-failure', { exact: true })).toHaveCount(0)
    await page.getByRole('button', { name: '查看请求 1 用量明细', exact: true }).click()
    await expect(page.getByRole('dialog', { name: '请求用量明细', exact: true }).getByText('145,376', { exact: true })).toBeVisible()
    await page.getByRole('button', { name: '关闭明细', exact: true }).click()
    await page.getByRole('tab', { name: '错误请求', exact: true }).click()
    await expect(page.getByText('synthetic-browser-failure', { exact: true })).toBeVisible()
    await expect(page.getByText('synthetic-browser-success', { exact: true })).toHaveCount(0)
    await expect(page.getByText('上游生成失败', { exact: true })).toBeVisible()
    await page.screenshot({ path: 'test-results-integration/real-backend-requests-error.png', fullPage: true, animations: 'disabled' })
    await page.getByRole('tab', { name: '用量明细', exact: true }).click()
    await page.screenshot({ path: 'test-results-integration/real-backend-requests-success.png', fullPage: true, animations: 'disabled' })
    await page.getByRole('button', { name: '供应商设置', exact: true }).click()
  } finally { await new Promise<void>(resolve => upstream.close(() => resolve())) }
  const stored = await page.evaluate(async () => (await fetch('/api/v1/credentials')).json())
  expect(stored).toEqual({ items: [{ provider_id: 'company', configured: true }] })
  expect(await page.content()).not.toContain('synthetic-integration-company-key')
  expect(await page.evaluate(() => [localStorage.length, sessionStorage.length])).toEqual([0, 0])
  await expect(page.getByRole('button', { name: '删除密钥 公司线路', exact: true })).toBeEnabled()
  await page.getByRole('button', { name: '删除密钥 公司线路', exact: true }).click()
  await page.getByRole('button', { name: '确认' }).click()
  await expect(page.getByText('密钥未配置', { exact: true })).toBeVisible()

  await page.reload()
  await expect(page.getByText('本地服务运行中')).toBeVisible()
  await expect(page.getByRole('button', { name: '刷新', exact: true })).toBeEnabled()
  await page.getByRole('button', { name: '供应商设置' }).click()
  await expect(page.getByLabel('连接超时（毫秒）')).toHaveValue('11000')
  await expect(page.getByText('密钥未配置', { exact: true })).toBeVisible()
  await page.screenshot({ path: 'test-results-integration/real-backend-settings.png', fullPage: true, animations: 'disabled' })

  await context.clearCookies()
  await page.getByRole('button', { name: '刷新', exact: true }).click()
  await expect(page.getByText('管理认证已失效', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '保存超时与并发' })).toBeDisabled()
})
