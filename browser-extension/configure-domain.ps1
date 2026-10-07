param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string]$AdminUrl
)

$ErrorActionPreference = 'Stop'
$uri = [Uri]$AdminUrl
$isLocalHttp = $uri.Scheme -eq 'http' -and @('127.0.0.1', 'localhost') -contains $uri.Host
if ($uri.Scheme -ne 'https' -and -not $isLocalHttp) {
    throw 'A public admin URL must use HTTPS.'
}
if (-not $uri.IsAbsoluteUri -or $uri.Query -or $uri.Fragment -or ($uri.AbsolutePath -notin @('', '/'))) {
    throw 'Enter an origin without a path, query or fragment, for example https://admin.example.com'
}

$manifestPath = Join-Path $PSScriptRoot 'manifest.json'
$manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
$matchPattern = '{0}://{1}/*' -f $uri.Scheme, $uri.Host

$hostPermissions = @($manifest.host_permissions)
if ($hostPermissions -notcontains $matchPattern) {
    $hostPermissions += $matchPattern
}
$manifest.host_permissions = $hostPermissions

$bridge = @($manifest.content_scripts) | Where-Object { @($_.js) -contains 'admin-bridge.js' } | Select-Object -First 1
if (-not $bridge) {
    throw 'admin-bridge.js was not found in manifest.json.'
}
$matches = @($bridge.matches)
if ($matches -notcontains $matchPattern) {
    $matches += $matchPattern
}
$bridge.matches = $matches

$json = $manifest | ConvertTo-Json -Depth 20
[IO.File]::WriteAllText($manifestPath, $json + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))
Write-Host "Admin origin enabled: $matchPattern" -ForegroundColor Green
Write-Host 'If the extension is already loaded, click Reload on the extensions page.'
