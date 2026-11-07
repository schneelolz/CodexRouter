import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, realpath, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import net from 'node:net';
import http from 'node:http';
import { checkMacOSPackage } from './check-macos-package.mjs';
import { cleanupSyntheticControl } from './synthetic-credentials.mjs';

if (process.platform!=='darwin') throw Error('macOS only');
const ix=process.argv.indexOf('--exe');
if (ix<0 || !process.argv[ix+1]) throw Error('node scripts/macos-smoke.mjs --exe <binary>');
const exe=await realpath(process.argv[ix+1]);
checkMacOSPackage(path.resolve(path.dirname(exe),'../..'));
const ui=path.resolve(path.dirname(exe),'../Resources/ui');
const root=await realpath(await mkdtemp(path.join(tmpdir(),'codexrouter-macos-smoke-')));
const children=new Set(), dirs=[], results=[];
const delay=ms=>new Promise(r=>setTimeout(r,ms));
async function bounded(p,ms=10000) {
 let timer; try { return await Promise.race([p,new Promise((_,reject)=>{timer=setTimeout(()=>reject(Error('owned process timeout')),ms)})]); } finally {clearTimeout(timer)}
}
async function port() {
 const s=net.createServer(); await new Promise((r,j)=>{s.once('error',j);s.listen(0,'127.0.0.1',r)});
 const p=s.address().port; await new Promise(r=>s.close(r)); return p;
}
async function data(name) { const d=path.join(root,name);await mkdir(d,{mode:0o700});dirs.push(d);return d }
function start(args) {
 const c=spawn(exe,args,{cwd:root,stdio:['pipe','pipe','pipe']}); children.add(c);
 let ended=false; const lines=[];
 c.stdout.on('data',()=>{}); c.stderr.on('data',b=>{if(lines.length<10)lines.push(String(b))});
 const exit=new Promise((r,j)=>{c.once('error',j);c.once('exit',(code,signal)=>{ended=true;children.delete(c);r({code,signal})})});
 return { c,exit,ended:()=>ended,diagnostic:()=>lines.join('').replace(/token=[^\s]+/g,'token=REDACTED') };
}
async function ready(p,owned) {
 for(let n=0;n<120;n++) {
  if(owned.ended()) throw Error(`startup failed: ${owned.diagnostic()}`);
  try {const r=await fetch(`http://127.0.0.1:${p}/healthz`,{signal:AbortSignal.timeout(300)});if(r.ok)return} catch {}
  await delay(50);
 }
 throw Error('health timeout');
}
try {
 const d=await data('foreground'), p=await port();
 const s=start(['serve','--ui-dir',ui,'--data-dir',d,'--port',String(p)]);
 await ready(p,s);
 assert.equal((await fetch(`http://127.0.0.1:${p}/`)).status,200);
 const badHost=await new Promise((resolve,reject)=>{
  const req=http.get({hostname:'127.0.0.1',port:p,path:'/api/v1/status',headers:{Host:'evil.example'}},res=>{res.resume();res.on('end',()=>resolve(res.statusCode))});req.once('error',reject);
 });
 assert.equal(badHost,403);
 const duplicate=start(['serve','--api-only','--data-dir',d,'--port',String(await port())]);
 assert.equal((await bounded(duplicate.exit)).code,1);
 assert.match(duplicate.diagnostic(),/持锁|锁定/);
 const occupied=await data('occupied');
 const conflict=start(['serve','--api-only','--data-dir',occupied,'--port',String(p)]);
 assert.equal((await bounded(conflict.exit)).code,1);
 assert.match(conflict.diagnostic(),/未更换端口或停止其他程序/);
 assert.equal((await fetch(`http://127.0.0.1:${p}/healthz`)).status,200);
 s.c.stdin.write('quit\n');assert.equal((await bounded(s.exit)).code,0);
 const again=start(['serve','--api-only','--data-dir',d,'--port',String(p)]);
 await ready(p,again);again.c.kill('SIGTERM');assert.equal((await bounded(again.exit)).code,0);
 results.push({check:'serve-ui-host-port-lock-quit-sigterm-restart',passed:true});
 const trayData=await data('tray'), trayPort=await port();
 const tray=start(['launch','--open=false','--data-dir',trayData,'--port',String(trayPort)]);
 await ready(trayPort,tray);await delay(500);assert.equal(tray.ended(),false);
 tray.c.kill('SIGTERM');assert.equal((await bounded(tray.exit)).code,0);
 const trayRestart=start(['launch','--open=false','--data-dir',trayData,'--port',String(trayPort)]);
 await ready(trayPort,trayRestart);trayRestart.c.kill('SIGINT');assert.equal((await bounded(trayRestart.exit)).code,0);
 results.push({check:'appkit-main-thread-tray-sigterm-sigint-restart',passed:true,menu_clicks:'manual pending'});
} finally {
 for(const c of children) c.kill('SIGTERM');
 // Wait for only the exact children created above, before removing fixtures.
 for(let n=0;children.size && n<200;n++)await delay(50);
 if(children.size)throw Error('owned child still running; fixtures retained');
 for(const d of dirs)await cleanupSyntheticControl(d,root);
 await mkdir('artifacts/verification/macos-smoke',{recursive:true});
 await writeFile('artifacts/verification/macos-smoke/results.json',JSON.stringify(results,null,2)+'\n');
 await rm(root,{recursive:true,force:true});
}
console.log(JSON.stringify(results,null,2));
