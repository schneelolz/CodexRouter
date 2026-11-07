import { realpath } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';

// Deletes only this run's synthetic control credential, never enumerates credentials.
export async function cleanupSyntheticControl(data, scratchRoot, profileID = '') {
  if (profileID && (process.platform !== 'darwin' || !/^[a-f0-9]{32}$/.test(profileID))) throw Error('Invalid synthetic profile ID');
  const root=await realpath(scratchRoot);
  const temporary=await realpath(tmpdir());
  const resolved=await realpath(data);
  if (path.dirname(root)!==temporary || !path.basename(root).startsWith('codexrouter-') || !resolved.startsWith(root+path.sep)) throw Error('Credential cleanup outside owned fixture');
  const normalized=process.platform==='win32'?path.normalize(resolved).toLowerCase():resolved;
  const hash=createHash('sha256').update(normalized).digest('hex');
  const service=`CodexRouter/${hash}`+(profileID?`/profile/${profileID}`:'');
  const p=process.platform==='darwin'
    ? spawnSync('/usr/bin/security',['delete-generic-password','-s',service,'-a','control'],{stdio:'ignore'})
    : spawnSync(path.join(process.env.SystemRoot,'System32','cmdkey.exe'),[`/delete:CodexRouter/${hash}/control`],{windowsHide:true,stdio:'ignore'});
  if (p.error || (p.status!==0 && !(process.platform==='darwin' && p.status===44))) throw Error('Synthetic control credential cleanup failed');
  if (process.platform==='darwin') {
    const check=spawnSync('/usr/bin/security',['find-generic-password','-s',service,'-a','control'],{stdio:'ignore'});
    if (check.error || check.status!==44) throw Error('Synthetic control credential still exists after cleanup');
  }
}
