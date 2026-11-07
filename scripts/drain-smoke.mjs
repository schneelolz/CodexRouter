import http from 'node:http';
import net from 'node:net';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, realpath, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import assert from 'node:assert/strict';
import { cleanupSyntheticControl } from './synthetic-credentials.mjs';
import { performance } from 'node:perf_hooks';

const exeIndex = process.argv.indexOf('--exe');
if (exeIndex < 0 || !process.argv[exeIndex + 1]) throw new Error('用法：node scripts/drain-smoke.mjs --exe <codexrouter.exe>');
const executable = await realpath(path.resolve(process.argv[exeIndex + 1]));
const scratchRoot = await mkdtemp(path.join(tmpdir(), 'codexrouter-drain-'));
const processes = new Set();
const credentials = [];
const results = [];
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function bounded(promise, milliseconds, name) {
  let timer;
  try {
    return await Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`${name}超时`)), milliseconds); })]);
  } finally { clearTimeout(timer); }
}
async function listen(server) {
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  return server.address().port;
}
async function freePort() {
  const server = net.createServer();
  const port = await listen(server);
  await new Promise(resolve => server.close(resolve));
  return port;
}
async function healthy(port, exited) {
  for (let attempt = 0; attempt < 100; attempt++) {
    if (exited()) throw new Error('本任务路由器在健康检查前退出');
    const ok = await new Promise(resolve => {
      const req = http.get({ hostname: '127.0.0.1', port, path: '/healthz', agent: false }, res => {
        res.resume(); res.on('end', () => resolve(res.statusCode === 200));
      });
      req.on('error', () => resolve(false));
      req.setTimeout(500, () => { req.destroy(); resolve(false); });
    });
    if (ok) return;
    await delay(50);
  }
  throw new Error('等待本地健康检查超时');
}
async function scenario(forceTimeout) {
  const name = forceTimeout ? 'five-second-forced-cancel' : 'active-stream-graceful-drain';
  const data = path.join(scratchRoot, name);
  await mkdir(data, { mode: 0o700 });
  const credential = { data, created: false };
  credentials.push(credential);
  let upstreamResponse, upstreamClosed = false, requestCount = 0;
  const sockets = new Set();
  const mock = http.createServer(async (req, res) => {
    req.resume();
    req.on('end', () => {
      requestCount++;
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' });
      res.flushHeaders();
      res.write('data: start\n\n');
      upstreamResponse = res;
      res.on('close', () => { upstreamClosed = true; });
    });
  });
  mock.on('connection', socket => { sockets.add(socket); socket.on('close', () => sockets.delete(socket)); });
  const upstreamPort = await listen(mock);
  await writeFile(path.join(data, 'state.json'), JSON.stringify({ version: 1, config: {
    version: 1, default_route: 'official', official_url: `http://127.0.0.1:${upstreamPort}`, company_url: '',
    connect_timeout_ms: 10000, header_timeout_ms: 30000, idle_timeout_ms: 120000,
    write_timeout_ms: 30000, max_concurrent: 16, upstreams_locked: false,
  }, bindings: {} }));
  const port = await freePort();
  const child = spawn(executable, ['serve', '--api-only', '--data-dir', data, '--port', String(port)], {
    cwd: scratchRoot, windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'],
  });
  processes.add(child);
  let exited = false, stderr = '';
  child.stdout.on('data', () => {});
  child.stderr.on('data', chunk => { if (stderr.length < 8192) stderr += chunk; });
  const exit = new Promise(resolve => {
    child.once('error', () => { exited = true; resolve({ code: null, spawn_error: true }); });
    child.once('exit', (code, signal) => { exited = true; resolve({ code, signal }); });
  });
  try {
    await healthy(port, () => exited);
    credential.created = true;
    let firstChunk;
    const first = new Promise(resolve => { firstChunk = resolve; });
    const stream = new Promise(resolve => {
      const request = http.request({ hostname: '127.0.0.1', port, method: 'POST', path: '/responses', agent: false,
        headers: { 'Content-Type': 'application/json', 'Thread-Id': 'synthetic-drain',
          'X-Codex-Turn-Metadata': JSON.stringify({ thread_id: 'synthetic-drain', session_id: 'synthetic-drain', history_initialization: 'new' }) } }, response => {
        const chunks = [];
        response.on('data', chunk => { chunks.push(chunk); firstChunk(); });
        const complete = kind => resolve({ kind, status: response.statusCode, complete: response.complete, body: Buffer.concat(chunks).toString('utf8') });
        response.on('end', () => complete('end'));
        response.on('aborted', () => complete('aborted'));
        response.on('error', () => complete('error'));
      });
      request.on('error', () => resolve({ kind: 'request-error', complete: false, body: '' }));
      request.end(JSON.stringify({ input: [{ role: 'user', content: 'synthetic' }] }));
    });
    await bounded(first, 5000, '等待首块');
    assert.ok(upstreamResponse, '首块应来自本轮模拟上游');
    const quitAt = performance.now();
    child.stdin.write('quit\n');
    if (!forceTimeout) {
      await delay(350);
      assert.equal(exited, false, '活动流未结束时主程序提前退出');
      upstreamResponse.end('event: done\ndata: complete\n\n');
    }
    const [outcome, exitResult] = await bounded(Promise.all([stream, exit]), 8000, '等待排空与退出');
    const elapsed = Math.round(performance.now() - quitAt);
    assert.equal(exitResult.code, 0, '退出码应为 0');
    assert.equal(outcome.status, 200);
    assert.equal(requestCount, 1, '路由器不能重放上游请求');
    assert.ok(elapsed >= 300, '应等待活动流排空');
    if (forceTimeout) {
      assert.ok(elapsed >= 4500 && elapsed <= 8000, '限时退出应在约 5 秒触发');
      assert.equal(outcome.complete, false, '超时流应明确截断，不能伪造正常结束');
      assert.ok(outcome.kind === 'aborted' || outcome.kind === 'error');
      assert.equal(outcome.body, 'data: start\n\n');
      for (let attempt = 0; !upstreamClosed && attempt < 40; attempt++) await delay(25);
      assert.equal(upstreamClosed, true, '限时退出应取消上游连接');
    } else {
      assert.equal(outcome.kind, 'end');
      assert.equal(outcome.complete, true);
      assert.equal(outcome.body, 'data: start\n\nevent: done\ndata: complete\n\n');
    }
    results.push({ check: name, passed: true, elapsed_after_quit_ms: elapsed, physical_upstream_requests: requestCount,
      stream_complete: outcome.complete, exit_code: exitResult.code, stderr_categories: ['error', 'timeout'].filter(category => stderr.toLowerCase().includes(category)) });
  } finally {
    if (!exited) { child.kill(); await bounded(exit, 2000, '清理本任务进程'); }
    processes.delete(child);
    mock.closeAllConnections(); for (const socket of sockets) socket.destroy();
    await new Promise(resolve => mock.close(resolve));
  }
}
let cleanupError;
try {
  await scenario(false);
  await scenario(true);
} finally {
  for (const child of processes) child.kill();
  for (const credential of credentials) {
    try { await cleanupSyntheticControl(credential.data, scratchRoot); }
    catch (error) { if (credential.created) cleanupError = error; }
  }
  const outputDir = path.resolve('artifacts/verification', `drain-${new Date().toISOString().replace(/[:.]/g, '-')}`);
  await mkdir(outputDir, { recursive: true });
  await writeFile(path.join(outputDir, 'results.json'), JSON.stringify(results, null, 2) + '\n');
  if (path.dirname(scratchRoot) !== tmpdir() || !path.basename(scratchRoot).startsWith('codexrouter-drain-')) throw new Error('临时清理路径不在本任务指定父目录');
  await rm(scratchRoot, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
  console.log(JSON.stringify({ output: outputDir, results }, null, 2));
  if (cleanupError) throw cleanupError;
}
