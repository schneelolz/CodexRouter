#requires -Version 7.0
[CmdletBinding()]
param([Parameter(Mandatory)][string]$Executable)
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path $PSScriptRoot -Parent
$executablePath = (Resolve-Path -LiteralPath $Executable).Path
$scratchRoot = Join-Path ([IO.Path]::GetTempPath()) ('codexrouter-release-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratchRoot | Out-Null
$isolatedExe = Join-Path $scratchRoot 'codexrouter.exe'
Copy-Item -LiteralPath $executablePath -Destination $isolatedExe
$results = [Collections.Generic.List[object]]::new()
$processes = [Collections.Generic.List[object]]::new()
$dataDirectories = [Collections.Generic.List[string]]::new()
$httpHandler = [Net.Http.HttpClientHandler]::new()
$httpHandler.UseProxy = $false
$client = [Net.Http.HttpClient]::new($httpHandler)
$client.Timeout = [TimeSpan]::FromSeconds(10)
function Free-Port {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try { return $listener.LocalEndpoint.Port } finally { $listener.Stop() }
}
function Start-Isolated([string[]]$Arguments) {
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $isolatedExe
    $info.WorkingDirectory = $scratchRoot
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $info.Environment['PATH'] = "$env:SystemRoot\System32;$env:SystemRoot"
    foreach ($argument in $Arguments) { $info.ArgumentList.Add($argument) }
    $process = [Diagnostics.Process]::Start($info)
    $entry = @{ process = $process; output = $process.StandardOutput.ReadToEndAsync(); error = $process.StandardError.ReadToEndAsync() }
    $processes.Add($entry)
    return $entry
}
function Wait-Healthy($Entry, [string]$BaseURL) {
    $healthDeadline = [DateTime]::UtcNow.AddSeconds(30)
    while ([DateTime]::UtcNow -lt $healthDeadline) {
        if ($Entry.process.HasExited) { throw "本任务进程启动失败：$($Entry.error.GetAwaiter().GetResult())" }
        try {
            $response = $client.GetAsync("$BaseURL/healthz").GetAwaiter().GetResult()
            try { if ([int]$response.StatusCode -eq 200) { return } } finally { $response.Dispose() }
        } catch [Net.Http.HttpRequestException] { }
        catch [Threading.Tasks.TaskCanceledException] { }
        Start-Sleep -Milliseconds 50
    }
    throw '等待本地健康检查超时。'
}
function Assert-HTTP([string]$URL, [int]$Status, [string]$Marker = '') {
    $response = $client.GetAsync($URL).GetAwaiter().GetResult()
    try {
        if ([int]$response.StatusCode -ne $Status) { throw "HTTP 状态不符：$URL" }
        if ($Marker -and -not $response.Content.ReadAsStringAsync().GetAwaiter().GetResult().Contains($Marker)) { throw "页面标记不符：$URL" }
    } finally { $response.Dispose() }
}
function Stop-Isolated($Entry) {
    if (-not $Entry.process.HasExited) {
        $Entry.process.StandardInput.WriteLine('quit')
        $Entry.process.StandardInput.Flush()
        if (-not $Entry.process.WaitForExit(8000)) { $Entry.process.Kill(); $Entry.process.WaitForExit(); throw '本任务进程未在限定时间内退出。' }
    }
    if ($Entry.process.ExitCode -ne 0) { throw "本任务进程退出失败：$($Entry.error.GetAwaiter().GetResult())" }
}
try {
    $data = Join-Path $scratchRoot 'data'
    $dataDirectories.Add($data)
    $port = Free-Port
    $baseURL = "http://127.0.0.1:$port"
    $api = Start-Isolated @('serve', '--api-only', '--data-dir', $data, '--port', "$port")
    Wait-Healthy $api $baseURL
    Assert-HTTP "$baseURL/" 404
    Assert-HTTP "$baseURL/api/v1/config" 401
    $results.Add(@{ check = '无 Go/Node/Python PATH 的干净目录 API-only 启动及认证'; passed = $true })
    $second = Start-Isolated @('serve', '--api-only', '--data-dir', $data, '--port', "$(Free-Port)")
    if (-not $second.process.WaitForExit(5000) -or $second.process.ExitCode -eq 0) { throw '同数据目录第二实例未拒绝启动。' }
    Assert-HTTP "$baseURL/healthz" 200
    $results.Add(@{ check = '第二实例拒绝且原实例仍可用'; passed = $true })
    $otherData = Join-Path $scratchRoot 'other-data'
    $dataDirectories.Add($otherData)
    $collision = Start-Isolated @('serve', '--api-only', '--data-dir', $otherData, '--port', "$port")
    if (-not $collision.process.WaitForExit(5000) -or $collision.process.ExitCode -eq 0) { throw '端口冲突未拒绝启动。' }
    Assert-HTTP "$baseURL/healthz" 200
    $results.Add(@{ check = '端口冲突拒绝且原实例仍可用'; passed = $true })
    Stop-Isolated $api
    $ui = Join-Path $scratchRoot 'ui'
    New-Item -ItemType Directory -Path $ui | Out-Null
    '<!doctype html><html lang="zh-CN"><title>验收</title><body>static-version-one</body></html>' | Set-Content -LiteralPath (Join-Path $ui 'index.html') -Encoding utf8
    $hashBefore = (Get-FileHash -LiteralPath $isolatedExe).Hash
    $hosted = Start-Isolated @('serve', '--ui-dir', $ui, '--data-dir', $data, '--port', "$port")
    Wait-Healthy $hosted $baseURL
    Assert-HTTP "$baseURL/" 200 'static-version-one'
    '<!doctype html><html lang="zh-CN"><title>验收</title><body>static-version-two</body></html>' | Set-Content -LiteralPath (Join-Path $ui 'index.html') -Encoding utf8
    Assert-HTTP "$baseURL/" 200 'static-version-two'
    if ($hashBefore -ne (Get-FileHash -LiteralPath $isolatedExe).Hash) { throw '静态文件更新意外修改可执行文件。' }
    Assert-HTTP "$baseURL/api/v1/unknown" 401 'AUTH_REQUIRED'
    Stop-Isolated $hosted
    $results.Add(@{ check = '独立静态托管及运行时页面替换，无需重新编译'; passed = $true })
    $results.Add(@{ check = '前台 quit 限时正常退出'; passed = $true })
    $results | Format-Table -AutoSize
} finally {
    foreach ($entry in $processes) {
        if (-not $entry.process.HasExited) { $entry.process.Kill(); $entry.process.WaitForExit() }
        # 等待异步输出释放句柄，避免退出后的目录仍被占用。
        $entry.output.GetAwaiter().GetResult() | Out-Null
        $entry.error.GetAwaiter().GetResult() | Out-Null
        $entry.process.Dispose()
    }
    $client.Dispose()
    # 仅删除本轮独立目录产生的 control 凭据，不枚举凭据、不读取凭据值。
    foreach ($directory in $dataDirectories) {
        $normalized = [IO.Path]::GetFullPath($directory).TrimEnd('\').ToLowerInvariant()
        $digest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($normalized))).ToLowerInvariant()
        $deleteInfo = [Diagnostics.ProcessStartInfo]::new()
        $deleteInfo.FileName = "$env:SystemRoot\System32\cmdkey.exe"
        $deleteInfo.ArgumentList.Add("/delete:CodexRouter/$digest/control")
        $deleteInfo.UseShellExecute = $false
        $deleteInfo.CreateNoWindow = $true
        $deleteInfo.RedirectStandardOutput = $true
        $deleteInfo.RedirectStandardError = $true
        $deleteProcess = [Diagnostics.Process]::Start($deleteInfo)
        $deleteProcess.StandardOutput.ReadToEnd() | Out-Null
        $deleteProcess.StandardError.ReadToEnd() | Out-Null
        $deleteProcess.WaitForExit()
        $deleteProcess.Dispose()
    }
    $resultDir = Join-Path $repoRoot ('artifacts/verification/release-smoke-' + (Get-Date -Format 'yyyyMMdd-HHmmss-fff'))
    New-Item -ItemType Directory -Force -Path $resultDir | Out-Null
    $results | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $resultDir 'results.json') -Encoding utf8
    $resolvedScratch = [IO.Path]::GetFullPath($scratchRoot)
    $expectedParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')
    if ([IO.Path]::GetDirectoryName($resolvedScratch) -eq $expectedParent -and [IO.Path]::GetFileName($resolvedScratch).StartsWith('codexrouter-release-')) {
        # Windows runner可能短暂保留文件锁；限定重试，不忽略最终清理失败。
        for ($cleanupAttempt = 0; $cleanupAttempt -lt 10; $cleanupAttempt++) {
            try { Remove-Item -LiteralPath $resolvedScratch -Recurse -Force -ErrorAction Stop; break }
            catch {
                if ($cleanupAttempt -eq 9) { throw }
                Start-Sleep -Milliseconds 200
            }
        }
    } else { throw '临时清理目标不在本任务指定父目录内。' }
    Write-Host "发布启动验收记录：$resultDir"
}
