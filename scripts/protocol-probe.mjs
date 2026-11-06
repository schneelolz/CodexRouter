import http from 'node:http';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, rm, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createHash } from 'node:crypto';

// 仅使用临时身份、本地接收器和本任务创建的子进程；不读取用户认证。
const root = await mkdtemp(path.join(tmpdir(), 'codexrouter-protocol-'));
const home = path.join(root, 'home');
const work = path.join(root, 'work');
await mkdir(home); await mkdir(work);
await writeFile(path.join(home, 'auth.json'), JSON.stringify({ OPENAI_API_KEY: 'synthetic-local-probe-not-a-real-key' }));
const records = [];
const children = new Set();
const processIDs = [];
let currentCase = '';
let responseCounter = 0;
let acceptWebSockets = false;
let connectionCounter = 0;
const headerValues = ['thread-id', 'session-id', 'originator'];
const metadataFields = ['thread_id', 'session_id', 'history_initialization', 'forked_from_thread_id', 'thread_source', 'request_kind', 'agent_name'];
function identity(req) {
  let metadata = {}; try { metadata = JSON.parse(req.headers['x-codex-turn-metadata'] ?? '{}'); } catch {}
  return { ...Object.fromEntries(headerValues.filter(k => req.headers[k]).map(k => [k, req.headers[k]])),
    metadata: Object.fromEntries(metadataFields.filter(k => metadata[k] !== undefined).map(k => [k, metadata[k]])) };
}
const server = http.createServer(async (req, res) => {
  const chunks = []; for await (const chunk of req) chunks.push(chunk);
  let body = {}; try { body = JSON.parse(Buffer.concat(chunks).toString('utf8')); } catch {}
  const record = { case: currentCase, transport: 'http', method: req.method, path: new URL(req.url, 'http://localhost').pathname,
    header_names: Object.keys(req.headers).sort(), identity: identity(req),
    authorization_scheme: req.headers.authorization?.split(' ')[0] ?? 'none', body_keys: Object.keys(body).sort(),
    previous_response_id_present: Boolean(body.previous_response_id), input_count: Array.isArray(body.input) ? body.input.length : null,
    input_shape: Array.isArray(body.input) ? body.input.map(item => ({ type:item.type, role:item.role, has_call_id:Boolean(item.call_id) })) : [],
    metadata_field_names: Object.keys((() => { try { return JSON.parse(req.headers['x-codex-turn-metadata'] ?? '{}'); } catch { return {}; } })()).sort(),
    response_status: null, sse_completed: false };
  records.push(record);
  res.on('finish', () => { record.response_status = res.statusCode; });
  if (req.method === 'GET' && req.url.startsWith('/models')) { res.setHeader('content-type', 'application/json'); res.end(JSON.stringify({ models: [] })); return; }
  if (!req.url.split('?')[0].endsWith('/responses')) { res.writeHead(404); res.end('{}'); return; }
  const id = `resp_synthetic_${++responseCounter}`;
  const item = { id: `msg_${responseCounter}`, type: 'message', role: 'assistant', status: 'completed', content: [{ type: 'output_text', text: 'synthetic probe complete', annotations: [] }] };
  const response = { id, object: 'response', created_at: Math.floor(Date.now()/1000), status: 'completed', model: 'gpt-5.4', output: [item], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2, input_tokens_details: { cached_tokens: 0 }, output_tokens_details: { reasoning_tokens: 0 } } };
  res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' });
  let sequence = 0;
  const event = (type, extra) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, sequence_number: sequence++, ...extra })}\n\n`);
  event('response.created', { response: { ...response, status: 'in_progress', output: [] } });
  event('response.output_item.added', { output_index: 0, item: { ...item, status: 'in_progress', content: [] } });
  event('response.content_part.added', { item_id: item.id, output_index: 0, content_index: 0, part: { type: 'output_text', text: '', annotations: [] } });
  event('response.output_text.delta', { item_id: item.id, output_index: 0, content_index: 0, delta: 'synthetic probe complete' });
  event('response.output_text.done', { item_id: item.id, output_index: 0, content_index: 0, text: 'synthetic probe complete' });
  event('response.content_part.done', { item_id: item.id, output_index: 0, content_index: 0, part: item.content[0] });
  event('response.output_item.done', { output_index: 0, item });
  event('response.completed', { response }); record.sse_completed = true; res.end();
});
server.on('upgrade', (req, socket) => {
  const connectionId = ++connectionCounter;
  records.push({ case: currentCase, transport: 'websocket-upgrade', method: req.method, path: new URL(req.url, 'http://localhost').pathname,
    header_names: Object.keys(req.headers).sort(), identity: identity(req), connection_id: connectionId, authorization_scheme: req.headers.authorization?.split(' ')[0] ?? 'none', accepted: acceptWebSockets });
  if (!acceptWebSockets) { socket.end('HTTP/1.1 426 Upgrade Required\r\nConnection: close\r\nContent-Length: 0\r\n\r\n'); return; }
  const accept = createHash('sha1').update(req.headers['sec-websocket-key'] + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64');
  socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
  function send(data, opcode = 1) {
    const payload = Buffer.from(typeof data === 'string' ? data : JSON.stringify(data));
    let head; if (payload.length < 126) { head = Buffer.from([0x80 | opcode, payload.length]); }
    else { head = Buffer.alloc(4); head[0] = 0x80 | opcode; head[1] = 126; head.writeUInt16BE(payload.length, 2); }
    socket.write(Buffer.concat([head,payload]));
  }
  let buffer = Buffer.alloc(0);
  socket.on('error', () => {});
  socket.on('data', chunk => {
    buffer = Buffer.concat([buffer, chunk]);
    while (buffer.length >= 2) {
      const opcode = buffer[0] & 15, masked = (buffer[1] & 128) !== 0;
      let length = buffer[1] & 127, offset = 2;
      if (length === 126) { if (buffer.length < 4) return; length = buffer.readUInt16BE(2); offset = 4; }
      else if (length === 127) { if (buffer.length < 10) return; length = Number(buffer.readBigUInt64BE(2)); offset = 10; }
      if (length > 8*1024*1024) { socket.destroy(); return; }
      const maskOffset = offset; if (masked) offset += 4;
      if (buffer.length < offset + length) return;
      const payload = Buffer.from(buffer.subarray(offset, offset+length));
      if (masked) for (let i=0;i<payload.length;i++) payload[i] ^= buffer[maskOffset+(i%4)];
      buffer = buffer.subarray(offset+length);
      if (opcode === 8) { send('',8); socket.end(); return; }
      if (opcode === 9) { send(payload.toString(),10); continue; }
      if (opcode !== 1) continue;
      let body; try { body=JSON.parse(payload.toString()); } catch { socket.destroy(); return; }
      let frameMetadata={}; try{frameMetadata=JSON.parse(body.client_metadata?.['x-codex-turn-metadata'] ?? '{}');}catch{}
      const frameIdentity={thread_id:body.client_metadata?.thread_id,session_id:body.client_metadata?.session_id,metadata:Object.fromEntries(metadataFields.filter(k=>frameMetadata[k]!==undefined).map(k=>[k,frameMetadata[k]]))};
      records.push({ case: currentCase, transport: 'websocket-frame', path:'/responses', identity:identity(req), frame_identity:frameIdentity,connection_id:connectionId, body_keys:Object.keys(body).sort(), client_metadata_keys:Object.keys(body.client_metadata ?? {}).sort(), type:body.type, generate:body.generate, stream_id_present:Boolean(body.stream_id), previous_response_id_present:Boolean(body.previous_response_id), input_count:Array.isArray(body.input)?body.input.length:null,
        input_shape:Array.isArray(body.input)?body.input.map(item=>({type:item.type,role:item.role,has_call_id:Boolean(item.call_id)})):[] });
      const id=`resp_synthetic_${++responseCounter}`;
      const item={id:`msg_${responseCounter}`,type:'message',role:'assistant',status:'completed',content:[{type:'output_text',text:'synthetic probe complete',annotations:[]}]};
      const response={id,object:'response',created_at:Math.floor(Date.now()/1000),status:'completed',model:'gpt-5.4',output:[item],usage:{input_tokens:1,output_tokens:1,total_tokens:2,input_tokens_details:{cached_tokens:0},output_tokens_details:{reasoning_tokens:0}}};
      let sequence=0;
      const event=(type,extra)=>send({type,sequence_number:sequence++,...extra});
      event('response.created',{response:{...response,status:'in_progress',output:[]}});
      event('response.output_item.added',{output_index:0,item:{...item,status:'in_progress',content:[]}});
      event('response.content_part.added',{item_id:item.id,output_index:0,content_index:0,part:{type:'output_text',text:'',annotations:[]}});
      event('response.output_text.delta',{item_id:item.id,output_index:0,content_index:0,delta:'synthetic probe complete'});
      event('response.output_text.done',{item_id:item.id,output_index:0,content_index:0,text:'synthetic probe complete'});
      event('response.content_part.done',{item_id:item.id,output_index:0,content_index:0,part:item.content[0]});
      event('response.output_item.done',{output_index:0,item});
      event('response.completed',{response});
    }
  });
});
const sockets = new Set(); server.on('connection', socket => { sockets.add(socket); socket.on('close', () => sockets.delete(socket)); });
await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
const port = server.address().port;
const baseURL = `http://127.0.0.1:${port}`;
const exe = process.env.CODEX_PROBE_EXE ?? path.join(process.env.LOCALAPPDATA, 'Programs', 'OpenAI', 'Codex', 'bin', 'codex.exe');
const env = { ...process.env, CODEX_HOME: home, OPENAI_API_KEY: 'synthetic-local-probe-not-a-real-key', OTEL_SDK_DISABLED: 'true' };
for (const key of Object.keys(env)) if (/^(HTTP_PROXY|HTTPS_PROXY|ALL_PROXY|OPENAI_BASE_URL|CODEX_API_KEY|CHATGPT_ACCESS_TOKEN|CODEX_APP_SERVER_OPENAI_BASE_URL|CODEX_APP_SERVER_CHATGPT_BASE_URL)$/i.test(key)) delete env[key];
const cases = [];
async function run(name, action = [], extra = []) {
  currentCase = name;
  const args = ['--no-daemon', 'exec', ...action, '--skip-git-repo-check', '--ignore-user-config', '--ignore-rules', '--json', '-c', 'model_provider="openai"', '-c', `openai_base_url="${baseURL}"`, '-c', `chatgpt_base_url="${baseURL}"`, '-c', 'web_search="disabled"', '-c', 'analytics.enabled=false', '-c', 'feedback.enabled=false', ...extra, 'Return only the synthetic probe result. Do not use tools.'];
  const child = spawn(exe, args, { cwd: work, env, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
  processIDs.push(child.pid); children.add(child); child.once('exit', () => children.delete(child));
  let output = '', stderr = ''; child.stdout.on('data', d => { output += d; }); child.stderr.on('data', d => { stderr += d; });
  const timer = setTimeout(() => child.kill(), 25000);
  const code = await new Promise(resolve => { child.on('error', () => resolve('spawn-error')); child.on('exit', resolve); }); clearTimeout(timer);
  const events = output.split(/\r?\n/).flatMap(line => { try { return [JSON.parse(line)]; } catch { return []; } });
  const result = { case: name, exit_code: code, thread_ids: events.filter(e => e.type === 'thread.started').map(e => e.thread_id), event_types: events.map(e => e.type), request_count: records.filter(r => r.case === name).length,
    stderr_categories: [...new Set(['error','warn','websocket','login','model'].filter(s => stderr.toLowerCase().includes(s)))], builtin_override_rejected:stderr.includes('reserved built-in provider IDs') };
  cases.push(result); return result;
}
async function appServerProbe(transport) {
  currentCase = `app-server-${transport}-interleaved`;
  const before = records.length;
  const child = spawn(exe, ['app-server', '--stdio', '-c', 'model_provider="openai"', '-c', `openai_base_url="${baseURL}"`, '-c', `chatgpt_base_url="${baseURL}"`, '-c', 'web_search="disabled"', '-c', 'analytics.enabled=false', '-c', 'feedback.enabled=false'], { cwd: work, env, windowsHide:true, stdio:['pipe','pipe','pipe'] });
  processIDs.push(child.pid); children.add(child); child.once('exit', () => children.delete(child));
  let buffer='', stderr='', next=0;
  const pending=new Map(), turns=new Map(), completed=[];
  let terminalError;
  function failProbe(error) {
    terminalError ??= error;
    for (const p of pending.values()) p.reject(terminalError);
    for (const turn of turns.values()) turn.reject(terminalError);
    pending.clear(); turns.clear();
  }
  const exited = new Promise(resolve => {
    child.once('error', error => { failProbe(error); resolve(); });
    child.once('exit', () => { failProbe(new Error('app-server-exited')); resolve(); });
  });
  child.stdin.on('error', failProbe);
  child.stderr.on('data', d=>{stderr+=d;});
  child.stdout.on('data',chunk=>{
    buffer+=chunk;
    for (;;) {
      const lineEnd=buffer.indexOf('\n'); if (lineEnd<0) break;
      const line=buffer.slice(0,lineEnd); buffer=buffer.slice(lineEnd+1);
      let msg;try{msg=JSON.parse(line);}catch{continue;}
      if (msg.id && pending.has(msg.id)) { const p=pending.get(msg.id);pending.delete(msg.id);msg.error?p.reject(new Error(msg.error.message)):p.resolve(msg.result); }
      if (msg.method==='turn/completed') { const id=msg.params.turn.id; completed.push({thread_id:msg.params.threadId,turn_id:id,status:msg.params.turn.status}); turns.get(id)?.resolve();turns.delete(id); }
    }
  });
  const rpc=(method,params)=>terminalError ? Promise.reject(terminalError) : new Promise((resolve,reject)=>{const id=++next;pending.set(id,{resolve,reject});child.stdin.write(JSON.stringify({jsonrpc:'2.0',id,method,params})+'\n');});
  const waitTurn=id=>terminalError ? Promise.reject(terminalError) : completed.some(t=>t.turn_id===id)?Promise.resolve():new Promise((resolve,reject)=>turns.set(id,{resolve,reject}));
  const timer=setTimeout(()=>{failProbe(new Error('probe-timeout'));child.kill();},25000);
  try {
    await rpc('initialize',{clientInfo:{name:'codexrouter_protocol_probe',version:'1.0.0'},capabilities:{experimentalApi:true}});
    child.stdin.write(JSON.stringify({jsonrpc:'2.0',method:'initialized',params:{}})+'\n');
    const a=await rpc('thread/start',{cwd:work,modelProvider:'openai',approvalPolicy:'never',sandbox:'read-only'});
    const b=await rpc('thread/start',{cwd:work,modelProvider:'openai',approvalPolicy:'never',sandbox:'read-only'});
    const ta=await rpc('turn/start',{threadId:a.thread.id,input:[{type:'text',text:'Return synthetic result. Do not use tools.'}]});
    const tb=await rpc('turn/start',{threadId:b.thread.id,input:[{type:'text',text:'Return synthetic result. Do not use tools.'}]});
    await Promise.all([waitTurn(ta.turn.id),waitTurn(tb.turn.id)]);
    const ta2=await rpc('turn/start',{threadId:a.thread.id,input:[{type:'text',text:'Return second synthetic result. Do not use tools.'}]});
    await waitTurn(ta2.turn.id);
    cases.push({case:currentCase,thread_ids:[a.thread.id,b.thread.id],completed_turns:completed,request_count:records.length-before});
  } catch { cases.push({case:currentCase,error:'app-server-probe-failed',request_count:records.length-before}); }
  finally {clearTimeout(timer);child.kill();await exited;}
}
try {
  const versionChild = spawn(exe, ['--version'], { cwd: work, env, windowsHide: true, stdio: ['ignore', 'pipe', 'ignore'] });
  processIDs.push(versionChild.pid); children.add(versionChild); versionChild.once('exit', () => children.delete(versionChild));
  let actualVersion = '';
  versionChild.stdout.on('data', data => { if (actualVersion.length < 256) actualVersion += data; });
  const versionExit = await new Promise(resolve => { versionChild.once('error', () => resolve(-1)); versionChild.once('close', resolve); });
  if (versionExit !== 0 || !/^codex-cli 0\.166\.0\s*$/.test(actualVersion)) throw new Error('此探针仅验收 codex-cli 0.166.0');
  const first = await run('builtin-default');
  const second = await run('builtin-websockets-false', [], ['-c', 'model_providers.openai.supports_websockets=false']);
  await run('builtin-retry-override-rejected', [], ['-c', 'model_providers.openai.request_max_retries=0']);
  const thread = second.thread_ids[0] ?? first.thread_ids[0];
  if (thread) {
    await run('resume-after-client-restart', ['resume', thread], []);
    await run('fork-with-history', ['fork', thread], []);
    await run('second-independent-thread', [], []);
    await run('resume-original-after-other-thread', ['resume', thread], []);
    server.closeAllConnections(); for (const socket of sockets) socket.destroy();
    await new Promise(resolve=>server.close(resolve));
    await new Promise((resolve,reject)=>{server.once('error',reject);server.listen(port,'127.0.0.1',resolve);});
    await run('resume-after-receiver-restart', ['resume', thread], []);
  }
  await appServerProbe('sse');
  acceptWebSockets = true;
  await run('builtin-websocket-success');
  await appServerProbe('websocket');
  const checks={
    successful_expected_cases:cases.filter(c=>!c.case.endsWith('-rejected')).every(c=>c.exit_code===0 || (c.completed_turns?.length===3&&c.completed_turns.every(t=>t.status==='completed'))),
    builtin_retry_override_rejected:cases.find(c=>c.case==='builtin-retry-override-rejected')?.builtin_override_rejected===true,
    default_websocket_to_sse:records.some(r=>r.case==='builtin-default'&&r.transport==='websocket-upgrade'&&!r.accepted)&&records.some(r=>r.case==='builtin-default'&&r.transport==='http'&&r.method==='POST'),
    override_false_still_websocket:records.some(r=>r.case==='builtin-websockets-false'&&r.transport==='websocket-upgrade'),
    http_header_metadata_consistency:records.filter(r=>r.transport==='http'&&r.method==='POST').every(r=>r.identity['thread-id']===r.identity.metadata.thread_id&&r.identity['session-id']===r.identity.metadata.session_id),
    websocket_frame_identity_consistency:records.filter(r=>r.transport==='websocket-frame').every(r=>r.frame_identity.thread_id===r.identity['thread-id']&&r.frame_identity.thread_id===r.frame_identity.metadata.thread_id),
    resume_stable:cases.filter(c=>c.case.startsWith('resume-')).every(c=>c.thread_ids?.[0]===thread),
    fork_identifies_parent:records.some(r=>r.case==='fork-with-history'&&r.identity.metadata.history_initialization==='cold_fork'&&r.identity.metadata.forked_from_thread_id===thread),
  };
  if(Object.values(checks).some(ok=>!ok))process.exitCode=1;
  const hashID = id => typeof id === 'string' ? `sha256:${createHash('sha256').update(id).digest('hex')}` : id;
  function redact(value, key = '') {
    if (Array.isArray(value)) return value.map(item => /^(thread_ids|turn_ids)$/.test(key) ? hashID(item) : redact(item));
    if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([k,v]) => [k, redact(v,k)]));
    return /^(thread[-_]id|session[-_]id|turn_id|forked_from_thread_id|parent_thread_id|root_thread_id|session_tree_root_id)$/.test(key) ? hashID(value) : value;
  }
  const evidence = redact({ codex_version: '0.166.0', cli_sha256: createHash('sha256').update(await readFile(exe)).digest('hex'), provider: 'openai', synthetic_only: true, desktop_verified: false, port, process_ids: processIDs, isolated_home: home, isolated_work: work, checks, cases, records });
  const out = path.resolve(process.env.CODEX_PROBE_OUTPUT ?? `artifacts/verification/protocol-${new Date().toISOString().replaceAll(':','-')}/protocol-evidence.json`);
  await mkdir(path.dirname(out), { recursive: true }); await writeFile(out, JSON.stringify(evidence, null, 2)+'\n');
  console.log(JSON.stringify({ output: out, checks, cases: evidence.cases }, null, 2));
} finally {
  for (const child of children) child.kill();
  server.closeAllConnections(); for (const socket of sockets) socket.destroy(); await new Promise(resolve => server.close(resolve));
  if(path.dirname(root)===tmpdir()&&path.basename(root).startsWith('codexrouter-protocol-'))await rm(root,{recursive:true,force:true,maxRetries:10,retryDelay:200});
}
