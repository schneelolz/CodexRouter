import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export function checkMacOSIcon(app) {
  const plist = spawnSync('/usr/bin/plutil', ['-extract', 'CFBundleIconFile', 'raw', '-o', '-', path.join(app, 'Contents/Info.plist')], { encoding: 'utf8' });
  assert.equal(plist.status, 0, 'App bundle must declare an icon');
  assert.equal(plist.stdout.trim(), 'CodexRouter.icns');
  const icon = readFileSync(path.join(app, 'Contents/Resources/CodexRouter.icns'));
  assert.equal(icon.toString('ascii', 0, 4), 'icns');
  assert.equal(icon.readUInt32BE(4), icon.length, 'ICNS length mismatch');
  const representations = new Set();
  let offset = 8;
  while (offset < icon.length) {
    assert.ok(offset + 8 <= icon.length, 'Truncated ICNS entry');
    const length = icon.readUInt32BE(offset + 4);
    assert.ok(length > 8 && offset + length <= icon.length, 'Invalid ICNS entry size');
    representations.add(icon.toString('ascii', offset, offset + 4));
    offset += length;
  }
  // 16, 32, 128, 256, 512 points, with both standard and Retina representations.
  for (const type of ['ic04', 'ic05', 'ic07', 'ic08', 'ic09', 'ic10', 'ic11', 'ic12', 'ic13', 'ic14']) {
    assert.ok(representations.has(type), `Missing icon representation ${type}`);
  }
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  if (!process.argv[2]) throw Error('Usage: node scripts/check-macos-icon.mjs <CodexRouter.app>');
  checkMacOSIcon(path.resolve(process.argv[2]));
  console.log('App icon declaration and all 10 ICNS representations passed');
}
