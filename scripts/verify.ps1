#requires -Version 7.0
[CmdletBinding()]
param([switch]$InstallBrowser)
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path $PSScriptRoot -Parent
$goExe = Join-Path $env:LOCALAPPDATA 'CodexRouter/toolchains/go/bin/go.exe'
if (-not (Test-Path -LiteralPath $goExe)) { $goExe = (Get-Command go -ErrorAction Stop).Source }
$clangBin = Join-Path $env:LOCALAPPDATA 'CodexRouter/toolchains/llvm-mingw-20261020-ucrt-x86_64/bin'
if (-not (Test-Path -LiteralPath (Join-Path $clangBin 'clang.exe'))) { throw '缺少固定 LLVM-MinGW 20261020，尚不能执行 Windows race 检测。' }
$logDir = Join-Path $repoRoot ('artifacts/verification/' + (Get-Date -Format 'yyyyMMdd-HHmmss-fff'))
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$results = [Collections.Generic.List[object]]::new()
$previousPath = $env:PATH
$previousCGO = $env:CGO_ENABLED
$previousCC = $env:CC
$previousToolchain = $env:GOTOOLCHAIN
function Invoke-Checked([string]$Name, [string]$Executable, [string[]]$Arguments) {
    Write-Host "执行：$Name"
    $watch = [Diagnostics.Stopwatch]::StartNew()
    & $Executable @Arguments
    $code = $LASTEXITCODE
    $results.Add([ordered]@{ check = $Name; exit_code = $code; seconds = [math]::Round($watch.Elapsed.TotalSeconds, 2) })
    if ($code -ne 0) { throw "$Name 失败，退出码 $code。" }
}
Push-Location $repoRoot
Start-Transcript -Path (Join-Path $logDir 'output.txt') | Out-Null
try {
    $env:GOTOOLCHAIN = 'local'
    $env:PATH = $clangBin + [IO.Path]::PathSeparator + $env:PATH
    $env:CC = 'clang'
    $env:CGO_ENABLED = '1'
    Invoke-Checked '管理契约及生成一致性' 'node' @('scripts/contract-check.mjs')
    # 只检查源码包：artifacts 下保留了历史验收复制的 Go 源码，不属于本模块。
    Invoke-Checked 'go vet' $goExe @('vet', './cmd/...', './internal/...')
    Invoke-Checked 'go test' $goExe @('test', './cmd/...', './internal/...', '-count=1', '-timeout', '180s')
    Invoke-Checked 'go test -race' $goExe @('test', '-race', './cmd/...', './internal/...', '-count=1', '-timeout', '180s')
    Invoke-Checked 'npm ci' 'npm.cmd' @('--prefix', 'frontend', 'ci')
    Invoke-Checked '前端类型检查' 'npm.cmd' @('--prefix', 'frontend', 'run', 'typecheck')
    Invoke-Checked '前端单元测试' 'npm.cmd' @('--prefix', 'frontend', 'run', 'test')
    Invoke-Checked '前端生产构建' 'npm.cmd' @('--prefix', 'frontend', 'run', 'build')
    if ($InstallBrowser) { Invoke-Checked '安装测试 Chromium' 'npm.cmd' @('--prefix', 'frontend', 'exec', '--', 'playwright', 'install', 'chromium') }
    Invoke-Checked '前端浏览器测试' 'npm.cmd' @('--prefix', 'frontend', 'run', 'test:e2e')
    Invoke-Checked '真实 Go API 与生产页面联调' 'npm.cmd' @('--prefix', 'frontend', 'run', 'test:integration')
    Write-Host "检查通过，记录位于 $logDir"
} finally {
    $results | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $logDir 'results.json') -Encoding utf8
    Stop-Transcript | Out-Null
    $env:PATH = $previousPath
    $env:CC = $previousCC
    $env:CGO_ENABLED = $previousCGO
    $env:GOTOOLCHAIN = $previousToolchain
    Pop-Location
}
