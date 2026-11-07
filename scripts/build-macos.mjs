import { spawnSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdir, cp, writeFile, symlink, readdir, mkdtemp, rm } from 'node:fs/promises';
import path from 'node:path';
import { tmpdir } from 'node:os';

if (process.platform !== 'darwin') throw Error('macOS build requires macOS and Apple Command Line Tools');
const options = { out: 'artifacts/macos-native', version: '0.8.0-macos-dev', arch: process.arch === 'arm64' ? 'arm64' : 'amd64', skipInstall: false };
for (let i=2;i<process.argv.length;i++) {
  const a=process.argv[i];
  if (a==='--skip-install') options.skipInstall=true;
  else if (a==='--out' || a==='--version' || a==='--arch') {
    const key={ '--out':'out', '--version':'version', '--arch':'arch' }[a];
    if (!process.argv[++i]) throw Error(`Missing ${a}`);
    options[key]=process.argv[i];
  } else throw Error(`Unknown option ${a}`);
}
if (!['arm64','amd64'].includes(options.arch) || !/^[a-zA-Z0-9._-]+$/.test(options.version)) throw Error('Invalid architecture/version');
const shortVersion=options.version.match(/^\d+\.\d+\.\d+/)?.[0];
if (!shortVersion) throw Error('Version must begin with major.minor.patch');
const out=path.resolve(options.out);
if (existsSync(out)) throw Error('Output exists; choose a new directory (never merge old UI)');
const localGo=path.resolve('artifacts/toolchains/go/bin/go');
const go=process.env.GO_EXE ?? (existsSync(localGo)?localGo:'go');
const toolPath=path.isAbsolute(go)?path.dirname(go)+path.delimiter+process.env.PATH:process.env.PATH;
function run(command,args,extra={}) {
  const p=spawnSync(command,args,{stdio:'inherit',env:{...process.env,PATH:toolPath,GOTOOLCHAIN:'local',...extra}});
  if (p.error || p.status!==0) throw Error(`${command} failed (${p.status}): ${p.error?.code ?? ''}`);
}
run('xcrun',['--find','clang']);
run('node',['scripts/contract-check.mjs']);
if (!options.skipInstall) run('npm',['--prefix','frontend','ci']);
run('npm',['--prefix','frontend','run','typecheck']);
run('npm',['--prefix','frontend','run','build']);
const app=path.join(out,'CodexRouter.app');
const contents=path.join(app,'Contents');
await mkdir(path.join(contents,'MacOS'),{recursive:true});
await mkdir(path.join(contents,'Resources'),{recursive:true});
run(go,['build','-trimpath','-ldflags',`-X main.version=${options.version}`,'-o',path.join(contents,'MacOS','codexrouter'),'./cmd/codexrouter'],{
  GOOS:'darwin',GOARCH:options.arch,CGO_ENABLED:'1',CC:'clang',MACOSX_DEPLOYMENT_TARGET:'13.0',
});
const iconset=path.join(out,'CodexRouter.iconset');
run('xcrun',['swift','scripts/macos-icon.swift',iconset]);
run('/usr/bin/iconutil',['-c','icns',iconset,'-o',path.join(contents,'Resources','CodexRouter.icns')]);
await cp('frontend/dist',path.join(contents,'Resources','ui'),{recursive:true});
await cp('LICENSE',path.join(contents,'Resources','LICENSE'));
await cp('NOTICE',path.join(contents,'Resources','NOTICE'));
await mkdir(path.join(contents,'Resources','licenses'));
for (const file of (await readdir('docs')).filter(file => file.endsWith('-LICENSE.txt'))) {
  await cp(path.join('docs',file),path.join(contents,'Resources','licenses',file));
}
await writeFile(path.join(contents,'Info.plist'),`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>io.github.schneelolz.CodexRouter</string>
<key>CFBundleName</key><string>CodexRouter</string>
<key>CFBundleDisplayName</key><string>CodexRouter</string>
<key>CFBundleExecutable</key><string>codexrouter</string>
<key>CFBundleIconFile</key><string>CodexRouter.icns</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>${shortVersion}</string>
<key>CRBuildVersion</key><string>${options.version}</string>
<key>CFBundleVersion</key><string>1</string>
<key>LSMinimumSystemVersion</key><string>13.0</string>
<key>LSUIElement</key><true/>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
`);
// The Go linker's Mach-O signature alone does not seal a complete .app bundle.
// Sign only after every resource and Info.plist are finalized; never use a broad custom requirement.
run('/usr/bin/codesign',['--force','--sign','-','--identifier','io.github.schneelolz.CodexRouter','--timestamp=none',app]);
run('node',['scripts/check-macos-package.mjs',app]);
await symlink('CodexRouter.app/Contents/MacOS/codexrouter',path.join(out,'codexrouter'));
await writeFile(path.join(out,'build.json'),JSON.stringify({version:options.version,arch:options.arch,cgo:true,minimum_macos:'13.0',local_adhoc_bundle_signed:true,developer_id_signed:false,notarized:false},null,2)+'\n');
// Only the freshly assembled application goes into the archive, never runtime data.
const archive=path.join(out,`CodexRouter-darwin-${options.arch}.zip`);
run('/usr/bin/ditto',['-c','-k','--keepParent',app,archive]);
const unpacked=await mkdtemp(path.join(tmpdir(),'codexrouter-package-'));
try {
  run('/usr/bin/ditto',['-x','-k',archive,unpacked]);
  run('node',['scripts/check-macos-package.mjs',path.join(unpacked,'CodexRouter.app')]);
} finally { await rm(unpacked,{recursive:true,force:true}); }
console.log(`Built ${app}\nLocal ad-hoc bundle signature and archive verified. No installation, Developer ID signing or notarization performed.`);
