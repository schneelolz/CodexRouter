import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { mkdtemp, mkdir, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import net from 'node:net';
import { cleanupSyntheticControl } from './synthetic-credentials.mjs';
import { checkMacOSPackage } from './check-macos-package.mjs';

if(process.platform!=='darwin')throw Error('macOS only');
const index=process.argv.indexOf('--exe');
if(index<0 || !process.argv[index+1])throw Error('Usage: --exe <app binary>');
const exe=await realpath(process.argv[index+1]);
checkMacOSPackage(path.resolve(path.dirname(exe),'../..'));
const root=await realpath(await mkdtemp(path.join(tmpdir(),'codexrouter-profile-smoke-')));
const data=path.join(root,'data');await mkdir(data,{mode:0o700});
let profile='',legacyCreated=false;
const children=new Set(),results=[];
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
async function until(check,label){const end=Date.now()+10000;while(Date.now()<end){if(await check())return;await delay(50)}throw Error(label)}
const reservation=net.createServer();await new Promise((resolve,reject)=>{reservation.once('error',reject);reservation.listen(0,'127.0.0.1',resolve)});
const port=reservation.address().port;await new Promise(resolve=>reservation.close(resolve));
function command(args,code=0){
 const p=spawnSync(exe,[...args,'--data-dir',data],{encoding:'utf8',timeout:10000});
 assert.equal(p.error,undefined);assert.equal(p.status,code,`profile command failed: ${p.stderr}`);
 return p.stdout;
}
function start(){
 const child=spawn(exe,['serve','--api-only','--data-dir',data,'--port',String(port)],{cwd:root,stdio:['pipe','ignore','pipe']});
 children.add(child);let ended=false,diagnostic='';
 child.stderr.on('data',chunk=>{if(diagnostic.length<4096)diagnostic+=chunk});
 const exit=new Promise((resolve,reject)=>{child.once('error',reject);child.once('exit',code=>{ended=true;children.delete(child);resolve(code)})});
 return {child,exit,ended:()=>ended,diagnostic:()=>diagnostic};
}
async function ready(process){await until(async()=>{if(process.ended())throw Error(`owned startup failed: ${process.diagnostic()}`);try{return(await fetch(`http://127.0.0.1:${port}/healthz`,{signal:AbortSignal.timeout(300)})).ok}catch{return false}},'profile router startup timeout')}
async function stop(process){process.child.stdin.write('quit\n');await until(()=>process.ended(),'owned router quit timeout');assert.equal(await process.exit,0)}
try{
 const initial=start();await ready(initial);legacyCreated=true;
 const original=await readFile(path.join(data,'state.json'));
 command(['keychain-profile','preview']);
 command(['keychain-profile','prepare','--confirm']);
 const pending=JSON.parse(await readFile(path.join(data,'keychain-profile-pending.json'),'utf8'));profile=pending.id;
 assert.match(command(['keychain-profile','check']),/control: prepared_access=ok/);
 command(['keychain-profile','activate','--confirm'],1);
 assert.match(command(['keychain-check']),/control: access=ok/);
 assert.equal((await fetch(`http://127.0.0.1:${port}/healthz`)).status,200);
 await stop(initial);
 command(['keychain-profile','activate','--confirm']);
 assert.equal(Buffer.compare(original,await readFile(path.join(data,'state.json'))),0);
 assert.match(command(['keychain-check']),/control: access=ok/);
 const updated=start();await ready(updated);
 command(['keychain-profile','rollback','--confirm'],1);
 await stop(updated);
 command(['keychain-profile','rollback','--confirm']);
 assert.equal(Buffer.compare(original,await readFile(path.join(data,'state.json'))),0);
 const reverted=start();await ready(reverted);await stop(reverted);
 results.push({check:'prepare-while-running-activate-requires-instance-lock',passed:true});
 results.push({check:'new-profile-start-and-legacy-rollback-preserve-routing-state',passed:true});
}finally{
 for(const child of children)child.kill('SIGTERM');
 await until(()=>children.size===0,'owned profile smoke cleanup timeout');
 if(profile)await cleanupSyntheticControl(data,root,profile);
 if(legacyCreated)await cleanupSyntheticControl(data,root);
 await rm(root,{recursive:true,force:true});
 await mkdir('artifacts/verification/profile-migration',{recursive:true});
 await writeFile('artifacts/verification/profile-migration/results.json',JSON.stringify(results,null,2)+'\n');
}
console.log(JSON.stringify(results,null,2));
