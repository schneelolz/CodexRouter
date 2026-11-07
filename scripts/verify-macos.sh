#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "$(uname -s)" != Darwin ]]; then echo 'macOS required' >&2; exit 1; fi
export GO_EXE="${GO_EXE:-$PWD/artifacts/toolchains/go/bin/go}"
if [[ ! -x "$GO_EXE" ]]; then GO_EXE="$(command -v go)"; export GO_EXE; fi
export PATH="$(dirname "$GO_EXE"):$PATH"
export GOTOOLCHAIN=local CGO_ENABLED=1
install_browser=false
cli_probe=false
for arg in "$@"; do
 case "$arg" in
  --install-browser) install_browser=true ;;
  --with-cli-probe) cli_probe=true ;;
  *) echo "Unknown option: $arg" >&2; exit 1 ;;
 esac
done
results="artifacts/verification/macos-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$results"
xcrun clang -Wno-deprecated-declarations -framework Security -framework Foundation -framework AppKit \
 scripts/macos-native-menu-smoke.m internal/platform/native_darwin.m -o "$results/native-menu-smoke"
node --input-type=module -e 'import {spawnSync} from "node:child_process";const p=spawnSync(process.argv[1],[],{stdio:"inherit",timeout:10000});if(p.error||p.status!==0)process.exit(1)' "$results/native-menu-smoke" | tee "$results/native-menu.txt"
node scripts/macos-keychain-update-smoke.mjs
node scripts/contract-check.mjs
go mod verify
go vet ./cmd/... ./internal/...
go test ./cmd/... ./internal/... -count=1 -timeout 180s -json | tee "$results/go.jsonl"
go test -race ./cmd/... ./internal/... -count=1 -timeout 240s -json | tee "$results/race.jsonl"
if $cli_probe; then
 CODEXROUTER_CODEX_INTEGRATION=1 go test ./internal/router -run '^TestIsolatedCodexCLIRoutesNewResumeForkAndRestart$' -count=1 -v -timeout 180s | tee "$results/cli.txt"
fi
npm --prefix frontend ci
npm --prefix frontend run typecheck
npm --prefix frontend run test
npm --prefix frontend run build
if $install_browser; then (cd frontend && npx playwright install chromium); fi
free_port() {
 node --input-type=module -e 'import net from "node:net";const s=net.createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})'
}
CODEXROUTER_UI_TEST_PORT="$(free_port)" npm --prefix frontend run test:e2e
CODEXROUTER_E2E_PORT="$(free_port)" npm --prefix frontend run test:integration
bash scripts/build-macos.sh --skip-install --out "$results/build" --version 0.8.0-macos-dev
node scripts/macos-smoke.mjs --exe "$results/build/codexrouter"
node scripts/macos-profile-smoke.mjs --exe "$results/build/codexrouter"
node scripts/drain-smoke.mjs --exe "$results/build/codexrouter"
printf 'macOS verification completed: %s\n' "$results"
