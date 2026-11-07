// Explicit interactive acceptance: opens one new tab in the system browser.
// Proxy target is fixed to this run's router; no request bodies, headers, tokens or cookies are logged.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createServer, request as httpRequest } from 'node:http';
import { mkdtemp, mkdir, writeFile, readFile, realpath, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
import { cleanupSyntheticControl } from './synthetic-credentials.mjs';

if (process.platform !== 'darwin' || !process.argv.includes('--open-system-browser')) throw Error('macOS only; requires explicit --open-system-browser');
const index = process.argv.indexOf('--exe');
if (index < 0 || !process.argv[index + 1]) throw Error('Required: --exe <app binary>');
const executable = await realpath(process.argv[index + 1]);
const require = createRequire(path.resolve('frontend/package.json'));
const { chromium, expect } = require('@playwright/test');
const root = await realpath(await mkdtemp(path.join(tmpdir(), 'codexrouter-interactive-')));
const data = path.join(root, 'data'), home = path.join(root, 'codex-home');
for (const dir of [data, home, path.join(root, 'home')]) await mkdir(dir, { mode: 0o700 });
const original = '# Synthetic interactive acceptance only\nmodel_provider="openai"\n[mcp_servers.fixture]\nurl="https://example.invalid"\n';
await writeFile(path.join(home, 'config.toml'), original, { mode: 0o600 });
const output = path.resolve('artifacts/verification', `macos-interactive-${new Date().toISOString().replace(/[:.]/g, '-')}`);
await mkdir(output, { recursive: true });
const processes = new Set(), checks = [];
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(predicate, label, ms = 15000) {
 const end = Date.now() + ms;
 while (Date.now() < end) { if (await predicate()) return; await delay(80); }
 throw Error(label);
}
async function listen(server, port = 0) {
 await new Promise((resolve, reject) => { server.once('error', reject); server.listen(port, '127.0.0.1', resolve); });
 return server.address().port;
}
const reservation = createServer();
const routerPort = await listen(reservation);
await new Promise(resolve => reservation.close(resolve));
let sessionCookie = '', exchangeCount = 0, statusCount = 0;
const bridge = createServer((request, response) => {
 if (request.headers.host !== '127.0.0.1:18789' || (request.headers.origin && request.headers.origin !== 'http://127.0.0.1:18789')) {
  response.writeHead(403); response.end(); return;
 }
 const upstream = httpRequest({ hostname: '127.0.0.1', port: routerPort, method: request.method, path: request.url,
  headers: { ...request.headers, host: `127.0.0.1:${routerPort}` } }, incoming => {
  if (request.url === '/api/v1/auth/exchange' && incoming.statusCode === 200) {
   const cookie = incoming.headers['set-cookie']?.find(value => value.startsWith('codexrouter_session='));
   if (cookie) { sessionCookie = cookie.split(';')[0].slice('codexrouter_session='.length); exchangeCount++; }
  }
  if (request.url === '/api/v1/status' && incoming.statusCode === 200) statusCount++;
  response.writeHead(incoming.statusCode, incoming.headers); incoming.pipe(response);
 });
 upstream.on('error', () => { if (!response.headersSent) response.writeHead(503); response.end(); });
 request.on('aborted', () => upstream.destroy());
 response.on('close', () => upstream.destroy());
 request.pipe(upstream);
});
let browserPort;
try { browserPort = await listen(bridge, 18789); }
catch (error) { await rm(root, { recursive: true, force: true }); throw error; }
const base = `http://127.0.0.1:${browserPort}`;
const environment = { PATH: process.env.PATH, HOME: process.env.HOME, CODEX_HOME: home, TMPDIR: root, LANG: 'en_US.UTF-8' };
function start(args) {
 const child = spawn(executable, args, { cwd: root, env: environment, stdio: ['ignore', 'ignore', 'pipe'] });
 let diagnostic = '';
 child.stderr.on('data', chunk => { if (diagnostic.length < 4096) diagnostic += chunk; });
 processes.add(child);
 let ended = false;
 const exit = new Promise((resolve, reject) => {
  child.once('error', reject);
  child.once('exit', code => { ended = true; processes.delete(child); resolve(code); });
 });
 return { child, exit, ended: () => ended, diagnostic: () => diagnostic };
}
let browser;
try {
 const router = start(['launch', '--open=false', '--data-dir', data, '--port', String(routerPort), '--dev-origin', base]);
 await until(async () => {
  if (router.ended()) throw Error(`isolated router exited before health check: ${router.diagnostic()}`);
  try { return (await fetch(`http://127.0.0.1:${routerPort}/healthz`, { signal: AbortSignal.timeout(300) })).ok; } catch { return false; }
 }, 'router startup timeout');
 // Duplicate launch authenticates to the already owned instance, which invokes the real /usr/bin/open.
 const duplicate = start(['launch', '--open=false', '--data-dir', data, '--port', String(routerPort)]);
 let duplicateCode;
 duplicate.exit.then(code => { duplicateCode = code; });
 await until(() => duplicate.ended(), 'duplicate launch timeout', 25000);
 assert.equal(duplicateCode, 0);
 await until(() => exchangeCount === 1 && statusCount >= 1, 'system browser did not authenticate and read management status', 25000);
 checks.push({ check: 'duplicate-launch-real-system-browser-ui-authentication', passed: true, authenticated: true });
 // Cookie belongs only to this synthetic instance. Reuse it in an isolated automation browser;
 // do not connect automation to any running user browser or persist its storage state.
 browser = await chromium.launch({ headless: true });
 const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
 await context.addCookies([{ name: 'codexrouter_session', value: sessionCookie, domain: '127.0.0.1', path: '/api/v1/', httpOnly: true, sameSite: 'Strict' }]);
 sessionCookie = '';
 const page = await context.newPage();
 await page.goto(base);
 await expect(page.getByText('本地服务运行中')).toBeVisible();
 await page.getByRole('button', { name: '客户端与接入', exact: true }).click();
 await page.getByRole('button', { name: '接入 CodexRouter', exact: true }).click();
 const dialog = page.getByRole('dialog', { name: /确认(接入日常 Codex|恢复接入前配置)/ });
 await expect(dialog).toBeVisible();
 await expect(dialog).toContainText(path.join(home, 'config.toml'));
 await dialog.getByRole('button', { name: '确认接入', exact: true }).click();
 await expect(page.getByText('配置已接入', { exact: true })).toBeVisible();
 assert.match(await readFile(path.join(home, 'config.toml'), 'utf8'), /openai_base_url/);
 await page.getByRole('button', { name: '恢复接入前配置', exact: true }).click();
 await dialog.getByRole('button', { name: '确认恢复', exact: true }).click();
 await expect(page.getByText('尚未接入', { exact: true })).toBeVisible();
 assert.equal(await readFile(path.join(home, 'config.toml'), 'utf8'), original);
 checks.push({ check: 'production-binary-browser-daily-preview-apply-restore-synthetic-home', passed: true });
 await page.getByRole('button', { name: '独立测试', exact: true }).click();
 await expect(page.getByRole('button', { name: '启动测试桌面版', exact: true })).toBeDisabled();
 await page.getByRole('button', { name: '隔离启动检查', exact: true }).click();
 await expect(page.getByText('隔离启动检查通过（--version）；未启动交互代理，不代表真实上游验收', { exact: true })).toBeVisible({ timeout: 15000 });
 await page.screenshot({ path: path.join(output, 'isolated-clients.png'), fullPage: true });
 checks.push({ check: 'production-binary-native-cli-probe-and-desktop-block', passed: true });
 await browser.close(); browser = undefined;
 router.child.kill('SIGTERM');
 await until(() => router.ended(), 'router shutdown timeout');
 assert.equal(await router.exit, 0);
 checks.push({ check: 'interactive-owned-router-graceful-exit', passed: true });
} finally {
 if (browser) await browser.close();
 for (const child of processes) child.kill('SIGTERM');
 await until(() => processes.size === 0, 'owned child cleanup timeout');
 bridge.closeAllConnections(); await new Promise(resolve => bridge.close(resolve));
 await cleanupSyntheticControl(data, root);
 await rm(root, { recursive: true, force: true });
 await writeFile(path.join(output, 'results.json'), JSON.stringify({ checks, system_browser_tab: 'opened by test; no existing tab closed', physical_mouse_click: 'not tested; accessibility permission unavailable' }, null, 2) + '\n');
}
console.log(JSON.stringify({ output, checks }, null, 2));
