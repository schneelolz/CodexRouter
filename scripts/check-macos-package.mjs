import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { checkMacOSIcon } from './check-macos-icon.mjs';

export function checkMacOSPackage(app) {
  checkMacOSIcon(app);
  const result = spawnSync('/usr/bin/codesign', ['--verify', '--strict', '--verbose=2', app], { encoding: 'utf8', timeout: 15000 });
  assert.equal(result.error, undefined, 'Unable to check app signature');
  assert.equal(result.status, 0, `Invalid app bundle signature: ${result.stderr.trim()}`);
  const identity = spawnSync('/usr/bin/codesign', ['-d', '--verbose=2', app], { encoding: 'utf8', timeout: 15000 });
  assert.equal(identity.status, 0, 'Unable to inspect signature');
  assert.match(identity.stderr, /^Identifier=io\.github\.schneelolz\.CodexRouter$/m, 'Unexpected signing identifier');
  // Verification proves local bundle integrity. It is not Gatekeeper, Developer ID or notarization acceptance.
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  if (!process.argv[2]) throw Error('Usage: node scripts/check-macos-package.mjs <CodexRouter.app>');
  checkMacOSPackage(path.resolve(process.argv[2]));
  console.log('App icon and strict code/resource signature verification passed (local integrity only)');
}
