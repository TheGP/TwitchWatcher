param(
    [string]$ProxyUrl,
    [string]$FirefoxProfile = "$env:APPDATA\Mozilla\Firefox\Profiles\6tpb3qyy.default-release"
)

$ErrorActionPreference = 'Stop'
$twitchEnvPath = 'C:\Users\gp\apps\twitch\.env'
$discoveryEnvPath = 'C:\Users\gp\apps\DiscoveryProject\.env'

function Get-EnvValue([string]$Path, [string]$Key) {
    $line = Get-Content -LiteralPath $Path | Where-Object { $_ -like "$Key=*" } | Select-Object -First 1
    if (-not $line) { throw "Missing $Key in $Path" }
    return $line.Substring($Key.Length + 1).Trim().Trim('"', "'")
}

if (-not (Get-Command sqlite3.exe -ErrorAction SilentlyContinue)) {
    throw 'sqlite3.exe is required to read Firefox cookies.'
}

$cookieDb = Join-Path $FirefoxProfile 'cookies.sqlite'
if (-not (Test-Path -LiteralPath $cookieDb)) { throw "Firefox cookie database not found: $cookieDb" }

$sql = "SELECT name,value,host AS domain,path,isSecure AS secure,isHttpOnly AS http_only,expiry AS expires FROM moz_cookies WHERE host IN ('.twitch.tv','twitch.tv','www.twitch.tv') AND expiry > (CAST(strftime('%s','now') AS INTEGER)*1000)"
$cookieJson = & sqlite3.exe -json $cookieDb $sql
if ($LASTEXITCODE -ne 0) { throw 'Could not read Firefox cookies.' }
$cookies = @(($cookieJson -join [Environment]::NewLine) | ConvertFrom-Json)
if (-not ($cookies | Where-Object { $_.name -eq 'auth-token' })) {
    throw 'Firefox profile has no active Twitch auth-token cookie. Log into Twitch in that Firefox profile first.'
}

if (-not $ProxyUrl) {
    $dataImpulse = Get-EnvValue $discoveryEnvPath 'PROXY_DATAIMPULSE_LP'
    $login, $password = $dataImpulse -split ':', 2
    if (-not $login -or -not $password) { throw 'Invalid PROXY_DATAIMPULSE_LP format.' }
    $proxyUser = [uri]::EscapeDataString($login + '__cr.us')
    $proxyPassword = [uri]::EscapeDataString($password)
    $ProxyUrl = "http://${proxyUser}:${proxyPassword}@gw.dataimpulse.com:10000"
}

$config = [ordered]@{
    channel = 'shaneboehm'
    poll_seconds = 30
    watch_seconds = 300
    twitch_token = ((Get-EnvValue $twitchEnvPath 'BOT_OAUTH') -replace '^oauth:', '')
    steel_api_key = Get-EnvValue $discoveryEnvPath 'STEEL_API_KEY'
    telegram_bot = ((Get-EnvValue $twitchEnvPath 'DEVELOPER_TELEGRAM_BOT_TOKEN') -replace '^bot', '')
    telegram_chat = Get-EnvValue $twitchEnvPath 'DEVELOPER_TELEGRAM_CHAT_ID'
    proxy_url = $ProxyUrl
    cookies = @($cookies | ForEach-Object {
        [ordered]@{
            name = $_.name
            value = $_.value
            domain = $_.domain
            path = $_.path
            secure = [bool]$_.secure
            http_only = [bool]$_.http_only
            expires = [long]$_.expires
        }
    })
}

$output = Join-Path $PSScriptRoot 'config.json'
$config | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $output -Encoding utf8
Write-Host "Wrote private config.json with $($cookies.Count) Twitch cookies."
