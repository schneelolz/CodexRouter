import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtemp, mkdir, cp, readFile, rm, realpath, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createHash } from 'node:crypto';

if (process.platform !== 'darwin') throw Error('macOS only');
const root = await realpath(await mkdtemp(path.join(tmpdir(), 'codexrouter-keychain-update-')));
const keychain = path.join(root, 'synthetic.keychain-db');
const active = path.join(root, 'active');
const before = spawnSync('/usr/bin/security', ['list-keychains', '-d', 'user'], { encoding: 'utf8' });
assert.equal(before.status, 0);
const results = [];
function run(file, args) {
 const p = spawnSync(file, args, { encoding: 'utf8', timeout: 15000 });
 if (p.error || p.status !== 0) throw Error('Synthetic Keychain update test process failed');
 return p.stdout.trim();
}
function operation(action, binary = active) {
 const match = /^variant=(\d+) status=(-?\d+)$/.exec(run(binary, [action, keychain]));
 assert.ok(match, 'Invalid fixed diagnostic output');
 return Number(match[2]);
}
let created = false;
try {
 for (const variant of [1, 2]) {
  run('xcrun', ['clang', '-Wno-deprecated-declarations', `-DBUILD_VARIANT=${variant}`, '-framework', 'Security', '-framework', 'Foundation', '-framework', 'AppKit', 'scripts/macos-keychain-update-fixture.m', 'internal/platform/native_darwin.m', '-o', path.join(root, `variant-${variant}`)]);
 }
 const hashes = await Promise.all([1, 2].map(async n => createHash('sha256').update(await readFile(path.join(root, `variant-${n}`))).digest('hex')));
 assert.notEqual(hashes[0], hashes[1]);
 await cp(path.join(root, 'variant-1'), active);
 assert.equal(operation('create'), 0); created = true;
 assert.equal(operation('read'), 0);
 assert.equal(operation('check'), 0);
 // Moving the exact build after explicit authorization must preserve its access.
 assert.equal(operation('check', path.join(root, 'variant-1')), 0);
 results.push({ check: 'same-build-access-survives-copy-to-another-path', passed: true });
 await cp(path.join(root, 'variant-2'), active);
 const denied = operation('read');
 assert.ok([-25308, -25293].includes(denied), `Changed ad-hoc build expected access denial, received ${denied}`);
 assert.ok([-25308, -25293].includes(operation('check')), 'Noninteractive check must honor the same access restriction');
 results.push({ check: 'changed-adhoc-binary-cannot-silently-read-existing-credential', passed: true, os_status: denied });
 await cp(path.join(root, 'variant-1'), active);
 assert.equal(operation('read'), 0);
 results.push({ check: 'original-binary-still-reads-after-denied-update', passed: true });
 assert.equal(operation('delete'), 0); created = false;
} finally {
 if (created) {
  await cp(path.join(root, 'variant-1'), active);
  assert.equal(operation('delete'), 0, 'Synthetic keychain cleanup failed');
 }
 const after = spawnSync('/usr/bin/security', ['list-keychains', '-d', 'user'], { encoding: 'utf8' });
 assert.equal(after.status, 0);
 assert.equal(after.stdout, before.stdout, 'User keychain search list changed');
 await rm(root, { recursive: true, force: true });
 await mkdir('artifacts/verification/keychain-update', { recursive: true });
 await writeFile('artifacts/verification/keychain-update/results.json', JSON.stringify(results, null, 2) + '\n');
}
console.log(JSON.stringify(results, null, 2));
