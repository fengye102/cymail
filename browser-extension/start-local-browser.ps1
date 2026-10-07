$edgePath = "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
if (-not (Test-Path -LiteralPath $edgePath)) {
    $edgePath = "C:\Program Files\Microsoft\Edge\Application\msedge.exe"
}
if (-not (Test-Path -LiteralPath $edgePath)) {
    throw "Microsoft Edge was not found."
}

$extensionRoot = $PSScriptRoot
$profileRoot = Join-Path $env:TEMP "CYMail-Edge-Profile"
$arguments = @(
    "--user-data-dir=$profileRoot",
    "--no-first-run",
    "--disable-extensions-except=$extensionRoot",
    "--load-extension=$extensionRoot",
    "--new-window",
    "http://127.0.0.1:8080/login.html",
    "http://127.0.0.1:8082/pickup",
    "http://127.0.0.1:5000/login"
)

Start-Process -FilePath $edgePath -ArgumentList $arguments
Write-Output "CYMail browser extension started in an isolated Edge profile."
