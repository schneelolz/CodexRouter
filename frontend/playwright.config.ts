import { defineConfig } from '@playwright/test'

// 测试可选独立端口，避免停止用户已有的预览进程。
const port = process.env.CODEXROUTER_UI_TEST_PORT ?? '18789'
if (!/^\d{4,5}$/.test(port) || Number(port) < 1024 || Number(port) > 65535) throw new Error('测试端口无效')
const baseURL = `http://127.0.0.1:${port}`

export default defineConfig({
  testDir: './e2e',
  testIgnore: '**/integration.spec.ts',
  fullyParallel: false,
  workers: 1,
  reporter: 'list',
  use: { baseURL, browserName: 'chromium', trace: 'retain-on-failure' },
  webServer: { command: `npm run dev -- --port ${port}`, url: baseURL, reuseExistingServer: false, timeout: 30000 },
  outputDir: './test-results',
})
