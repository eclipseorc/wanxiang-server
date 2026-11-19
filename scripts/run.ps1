# 万象服务一键管理脚本（Windows / PowerShell）
#
# 用法（在项目根目录执行）：
#   powershell -ExecutionPolicy Bypass -File scripts\run.ps1 build        编译全部二进制
#   powershell -ExecutionPolicy Bypass -File scripts\run.ps1 start        启动四个独立进程（分离模式）
#   powershell -ExecutionPolicy Bypass -File scripts\run.ps1 stop         停止四个独立进程
#   powershell -ExecutionPolicy Bypass -File scripts\run.ps1 restart      先 stop 再 start
#   powershell -ExecutionPolicy Bypass -File scripts\run.ps1 status       查看四个服务状态
#   powershell -ExecutionPolicy Bypass -File scripts\run.ps1 single       启动单进程聚合入口（聚合模式）
#   powershell -ExecutionPolicy Bypass -File scripts\run.ps1 single-stop  停止聚合入口
#
# 也可双击 scripts\start.bat / stop.bat（内部封装了本脚本）。
#
# 两种运行模式：
#   - 分离模式（start/stop）：四个服务各自独立进程，一个挂了不影响其他，
#     但需要管理四个进程与四份日志。
#   - 聚合模式（single/single-stop）：单个 wanxiang 进程内起四个 goroutine，
#     supervisor 做 panic 隔离与自动重启，一条命令一个进程，日志集中在
#     logs\wanxiang.log。
#
# 说明：
#   - 只启动 bin\ 下已编译好的二进制（.exe），编译请先执行 build。
#   - 所有服务从项目根目录运行，读取同一个 .env 与 SQLite。
#   - 分离模式日志写入 logs\<service>.log，PID 写入 data\pids\<service>.pid；
#     聚合模式日志写入 logs\wanxiang.log，PID 写入 data\pids\wanxiang.pid。

$ErrorActionPreference = 'Stop'

$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$BinDir = Join-Path $Root 'bin'
$PidDir = Join-Path $Root 'data\pids'
$LogDir = Join-Path $Root 'logs'

$Services = @('web-server', 'admin-server', 'websocket-server', 'scrcpy-server')
# 聚合入口：单个进程内启动全部四个服务。
$AggregateName = 'wanxiang'

function Ensure-Dirs {
    New-Item -ItemType Directory -Force -Path $PidDir | Out-Null
    New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
}

function Get-PidFile([string]$name) {
    return Join-Path $PidDir "$name.pid"
}

function Test-Running([string]$name) {
    $pidFile = Get-PidFile $name
    if (-not (Test-Path $pidFile)) { return $false }
    $pidValue = (Get-Content $pidFile -ErrorAction SilentlyContinue).Trim()
    if ([string]::IsNullOrEmpty($pidValue)) { return $false }
    $proc = Get-Process -Id ([int]$pidValue) -ErrorAction SilentlyContinue
    return ($null -ne $proc)
}

function Invoke-Build {
    Set-Location $Root
    New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
    Write-Host '开始编译四个服务...'
    foreach ($name in $Services) {
        Write-Host "  -> 编译 $name"
        $out = Join-Path $BinDir "$name.exe"
        go build -o $out "./cmd/$name"
        if ($LASTEXITCODE -ne 0) { throw "编译 $name 失败" }
    }
    Write-Host "  -> 编译 $AggregateName（聚合入口）"
    $aggOut = Join-Path $BinDir "$AggregateName.exe"
    go build -o $aggOut "./cmd/$AggregateName"
    if ($LASTEXITCODE -ne 0) { throw "编译 $AggregateName 失败" }
    Write-Host "编译完成，产物位于 $BinDir"
}

function Invoke-Start {
    Ensure-Dirs
    Set-Location $Root

    if (-not (Test-Path (Join-Path $Root '.env'))) {
        Write-Host '提示：未找到 .env，将使用默认配置（建议先 Copy-Item .env.example .env）'
    }

    foreach ($name in $Services) {
        $exe = Join-Path $BinDir "$name.exe"
        if (-not (Test-Path $exe)) {
            throw "找不到可执行文件 $exe，请先执行 build"
        }

        if (Test-Running $name) {
            $pidValue = (Get-Content (Get-PidFile $name)).Trim()
            Write-Host "[跳过] $name 已在运行 (pid $pidValue)"
            continue
        }

        $logFile = Join-Path $LogDir "$name.log"
        $errFile = Join-Path $LogDir "$name.err.log"
        # 清理代理相关的环境变量（HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/NO_PROXY，
        # 含大小写变体），避免 Start-Process 继承环境时因同名大小写键冲突抛异常。
        # 服务走内网直连、不使用代理，移除它们不影响运行。
        # 用 .NET 环境变量字典（Hashtable）而非 Env: 驱动器，规避 Env: 因键冲突损坏的情况。
        $envVars = [System.Environment]::GetEnvironmentVariables()
        $proxyKeys = @($envVars.Keys | Where-Object { $_ -match '(?i)(^|_)(http_proxy|https_proxy|all_proxy|no_proxy)$' })
        foreach ($k in $proxyKeys) {
            [System.Environment]::SetEnvironmentVariable($k, $null, 'Process')
        }
        $proc = Start-Process -FilePath $exe -WorkingDirectory $Root -RedirectStandardOutput $logFile -RedirectStandardError $errFile -WindowStyle Hidden -PassThru
        $proc.Id | Out-File -FilePath (Get-PidFile $name) -Encoding ascii
        Write-Host "[启动] $name (pid $($proc.Id))，日志：$logFile"
    }
    Write-Host '全部启动完成。查看状态：powershell -ExecutionPolicy Bypass -File scripts\run.ps1 status'
}

function Invoke-Stop {
    Ensure-Dirs
    foreach ($name in $Services) {
        if (Test-Running $name) {
            $pidValue = (Get-Content (Get-PidFile $name)).Trim()
            $proc = Get-Process -Id ([int]$pidValue) -ErrorAction SilentlyContinue
            if ($proc) {
                $proc.CloseMainWindow() | Out-Null
                if (-not $proc.WaitForExit(3000)) {
                    $proc.Kill()
                    Write-Host "[停止] $name (pid $pidValue，强制)"
                } else {
                    Write-Host "[停止] $name (pid $pidValue)"
                }
            }
        } else {
            Write-Host "[跳过] $name 未在运行"
        }
        Remove-Item (Get-PidFile $name) -ErrorAction SilentlyContinue
    }
    Write-Host '全部停止完成。'
}

function Invoke-Restart {
    Invoke-Stop
    Start-Sleep -Seconds 1
    Invoke-Start
}

function Invoke-Status {
    Ensure-Dirs
    $anyRunning = $false
    foreach ($name in $Services) {
        if (Test-Running $name) {
            $pidValue = (Get-Content (Get-PidFile $name)).Trim()
            Write-Host "  [运行中] $name (pid $pidValue)"
            $anyRunning = $true
        } else {
            Write-Host "  [已停止] $name"
        }
    }
    if (Test-Running $AggregateName) {
        $pidValue = (Get-Content (Get-PidFile $AggregateName)).Trim()
        Write-Host "  [运行中] $AggregateName 聚合入口 (pid $pidValue)"
        $anyRunning = $true
    }
    if (-not $anyRunning) { Write-Host '所有服务均未运行。' }
}

function Invoke-Single {
    Ensure-Dirs
    Set-Location $Root

    $exe = Join-Path $BinDir "$AggregateName.exe"
    if (-not (Test-Path $exe)) {
        throw "找不到聚合入口 $exe，请先执行 build"
    }

    if (Test-Running $AggregateName) {
        $pidValue = (Get-Content (Get-PidFile $AggregateName)).Trim()
        Write-Host "[跳过] $AggregateName 聚合入口已在运行 (pid $pidValue)"
        return
    }

    $logFile = Join-Path $LogDir "$AggregateName.log"
    $errFile = Join-Path $LogDir "$AggregateName.err.log"
    # 清理代理环境变量（见 Invoke-Start 注释）。
    $envVars = [System.Environment]::GetEnvironmentVariables()
    $proxyKeys = @($envVars.Keys | Where-Object { $_ -match '(?i)(^|_)(http_proxy|https_proxy|all_proxy|no_proxy)$' })
    foreach ($k in $proxyKeys) {
        [System.Environment]::SetEnvironmentVariable($k, $null, 'Process')
    }
    $proc = Start-Process -FilePath $exe -WorkingDirectory $Root -RedirectStandardOutput $logFile -RedirectStandardError $errFile -WindowStyle Hidden -PassThru
    $proc.Id | Out-File -FilePath (Get-PidFile $AggregateName) -Encoding ascii
    Write-Host "[启动] $AggregateName 聚合入口 (pid $($proc.Id))，日志：$logFile"
}

function Invoke-SingleStop {
    Ensure-Dirs
    if (Test-Running $AggregateName) {
        $pidValue = (Get-Content (Get-PidFile $AggregateName)).Trim()
        $proc = Get-Process -Id ([int]$pidValue) -ErrorAction SilentlyContinue
        if ($proc) {
            $proc.CloseMainWindow() | Out-Null
            if (-not $proc.WaitForExit(3000)) {
                $proc.Kill()
                Write-Host "[停止] $AggregateName 聚合入口 (pid $pidValue，强制)"
            } else {
                Write-Host "[停止] $AggregateName 聚合入口 (pid $pidValue)"
            }
        }
    } else {
        Write-Host "[跳过] $AggregateName 聚合入口未在运行"
    }
    Remove-Item (Get-PidFile $AggregateName) -ErrorAction SilentlyContinue
}

$cmd = $args[0]
switch ($cmd) {
    'build'        { Invoke-Build }
    'start'        { Invoke-Start }
    'stop'         { Invoke-Stop }
    'restart'      { Invoke-Restart }
    'status'       { Invoke-Status }
    'single'       { Invoke-Single }
    'single-stop'  { Invoke-SingleStop }
    default {
        Write-Host '用法：run.ps1 {build|start|stop|restart|status|single|single-stop}'
        exit 1
    }
}
