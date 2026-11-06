#requires -Version 7.0
[CmdletBinding()]
param(
    [string]$OutputDir = 'artifacts/build',
    [string]$Version = '0.1.0-test',
    [switch]$BackendOnly,
    [switch]$FrontendOnly,
    [switch]$SkipInstall
)
$ErrorActionPreference = 'Stop'
if ($BackendOnly -and $FrontendOnly) { throw 'BackendOnly 与 FrontendOnly 不能同时使用。' }
if ($Version -notmatch '^[0-9A-Za-z.+_-]+$') { throw '版本号只能包含字母、数字、点、加号、下划线和连字符。' }
$repoRoot = Split-Path $PSScriptRoot -Parent
$outputPath = if ([IO.Path]::IsPathRooted($OutputDir)) { [IO.Path]::GetFullPath($OutputDir) } else { [IO.Path]::GetFullPath((Join-Path $repoRoot $OutputDir)) }
if (-not $BackendOnly -and (Test-Path -LiteralPath (Join-Path $outputPath 'ui'))) { throw '输出目录已有 ui；请指定新的 OutputDir，避免混入旧静态文件。' }
$previousCGO = $env:CGO_ENABLED
$previousToolchain = $env:GOTOOLCHAIN
Push-Location $repoRoot
try {
    New-Item -ItemType Directory -Force -Path $outputPath | Out-Null
    if (-not $FrontendOnly) {
        $goExe = Join-Path $env:LOCALAPPDATA 'CodexRouter/toolchains/go/bin/go.exe'
        if (-not (Test-Path -LiteralPath $goExe)) { $goExe = (Get-Command go -ErrorAction Stop).Source }
        $env:GOTOOLCHAIN = 'local'
        $env:CGO_ENABLED = '0'
        & $goExe build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version" -o (Join-Path $outputPath 'codexrouter.exe') ./cmd/codexrouter
        if ($LASTEXITCODE -ne 0) { throw 'Go 构建失败。' }
        Write-Host "后端产物：$(Join-Path $outputPath 'codexrouter.exe')"
    }
    if (-not $BackendOnly) {
        if (-not $SkipInstall) {
            & npm.cmd --prefix frontend ci
            if ($LASTEXITCODE -ne 0) { throw 'npm ci 失败。' }
        }
        & npm.cmd --prefix frontend run typecheck
        if ($LASTEXITCODE -ne 0) { throw '前端类型检查失败。' }
        & npm.cmd --prefix frontend run build
        if ($LASTEXITCODE -ne 0) { throw '前端构建失败。' }
        Copy-Item -LiteralPath (Join-Path $repoRoot 'frontend/dist') -Destination (Join-Path $outputPath 'ui') -Recurse
        Write-Host "前端产物：$(Join-Path $outputPath 'ui')"
    }
} finally {
    $env:CGO_ENABLED = $previousCGO
    $env:GOTOOLCHAIN = $previousToolchain
    Pop-Location
}
