#requires -Version 7.0
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path $PSScriptRoot -Parent
$goExe = Join-Path $env:LOCALAPPDATA 'CodexRouter/toolchains/go/bin/go.exe'
if (-not (Test-Path -LiteralPath $goExe)) { $goExe = (Get-Command go -ErrorAction Stop).Source }
Push-Location $repoRoot
try {
    $names = & $goExe test ./internal/router -list '^TestFaultRecovery100Cycles$'
    if ($LASTEXITCODE -ne 0 -or $names -notcontains 'TestFaultRecovery100Cycles') { throw '未找到故障循环测试，不能把零测试当作通过。' }
    & $goExe test ./internal/router -run '^TestFaultRecovery100Cycles$' -count=1 -v
    if ($LASTEXITCODE -ne 0) { throw '100 次故障恢复测试失败。' }
} finally { Pop-Location }
