param(
    [switch]$Restart
)

$serverRoot = $PSScriptRoot
$serverExe = Join-Path $serverRoot "cymail-api.exe"
$dataDir = Join-Path $serverRoot "data"
$apiKeyFile = Join-Path $dataDir "internal-api-key.txt"
$storeFile = Join-Path $dataDir "fulfillment.json"
$logDir = Join-Path $serverRoot "logs"
$stdoutLog = Join-Path $logDir "cymail-api.out.log"
$stderrLog = Join-Path $logDir "cymail-api.err.log"

New-Item -ItemType Directory -Force -Path $dataDir, $logDir | Out-Null

$running = Get-Process -Name "cymail-api" -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -eq $serverExe }

if ($running -and -not $Restart) {
    Write-Output "CYMail API is already running (PID $($running.Id -join ', '))."
    exit 0
}

if ($running) {
    $running | Stop-Process -Force
    $running | Wait-Process -Timeout 10 -ErrorAction SilentlyContinue
}

$arguments = @(
    "-addr", "127.0.0.1:8081",
    "-data", $dataDir,
    "-api-key-file", $apiKeyFile,
    "-file-store", $storeFile,
    "-insecure-plaintext-storage",
    "-collect-interval", "30s",
    "-public-base-url", "http://127.0.0.1:8082"
)

$process = Start-Process -FilePath $serverExe `
    -ArgumentList $arguments `
    -WorkingDirectory $serverRoot `
    -WindowStyle Hidden `
    -RedirectStandardOutput $stdoutLog `
    -RedirectStandardError $stderrLog `
    -PassThru

Write-Output "CYMail API started (PID $($process.Id)) on http://127.0.0.1:8081."
