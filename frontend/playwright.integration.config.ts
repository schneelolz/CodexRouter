// Node.js 类型由 tsconfig 显式包含，配置文件与页面源码一起校验。
import { defineConfig } from '@playwright/test'
import { randomBytes } from 'node:crypto'
import { existsSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('../', import.meta.url))
const localGo = path.join(process.env.LOCALAPPDATA ?? '', 'CodexRouter/toolchains/go/bin/go.exe')
const go = process.env.GO_EXE ?? (existsSync(localGo) ? localGo : 'go')
process.env.CODEXROUTER_E2E_TOKEN ??= randomBytes(32).toString('hex')
const port = process.env.CODEXROUTER_E2E_PORT ?? '19962'
if (!/^\d{4,5}$/.test(port) || Number(port) < 1024 || Number(port) > 65535) throw new Error('测试端口无效')
const baseURL = `http://127.0.0.1:${port}`

export default defineConfig({
  testDir: './e2e',
  testMatch: '**/integration.spec.ts',
  fullyParallel: false,
  workers: 1,
  timeout: 45000,
  reporter: 'list',
  use: { baseURL, browserName: 'chromium', trace: 'off' },
  webServer: {
    command: `"${go}" test -count=1 -run "^TestBrowserFixtureServer$" -timeout 120s ./internal/router`,
    cwd: root,
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    timeout: 45000,
    env: { CODEXROUTER_E2E_TOKEN: process.env.CODEXROUTER_E2E_TOKEN, CODEXROUTER_E2E_PORT: port },
  },
  outputDir: './test-results-integration',
})
