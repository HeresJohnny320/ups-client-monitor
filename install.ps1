# Installs or updates UPS Monitor on Windows as a task that starts at boot.
# In an Administrator PowerShell:
#
#   irm https://raw.githubusercontent.com/HeresJohnny320/ups-client-monitor/main/install.ps1 | iex
#
# Running it again updates to the newest release and keeps your settings.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # the progress bar makes downloads very slow on older PowerShell

$repo = 'HeresJohnny320/ups-client-monitor'
$base = if ($env:UPS_MONITOR_DOWNLOAD_BASE) { $env:UPS_MONITOR_DOWNLOAD_BASE } else { "https://github.com/$repo/releases/latest/download" }

$identity = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $identity.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host 'Please run this from an Administrator PowerShell (right-click PowerShell, Run as administrator).'
    return
}

# 32-bit PowerShell on 64-bit Windows reports x86; PROCESSOR_ARCHITEW6432 has the real one.
$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($cpu) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    'x86'   { '386' }
    default { throw "Sorry, there's no build for this processor ($cpu)." }
}
$asset = "ups-monitor-windows-$arch.exe"

# Older Windows PowerShell may not offer TLS 1.2 by default, which GitHub requires.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$tmp = Join-Path $env:TEMP ('ups-monitor-install-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $exe = Join-Path $tmp $asset
    $sums = Join-Path $tmp 'SHA256SUMS.txt'
    Write-Host "Downloading $asset..."
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $exe
    Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS.txt" -OutFile $sums

    $want = ''
    foreach ($line in Get-Content $sums) {
        $parts = $line -split '\s+'
        if ($parts.Count -ge 2 -and $parts[1].TrimStart('*') -eq $asset) { $want = $parts[0].ToLower() }
    }
    $got = (Get-FileHash -Algorithm SHA256 -Path $exe).Hash.ToLower()
    if (-not $want -or $want -ne $got) { throw "The download doesn't match its checksum. Not installing it." }
    Write-Host 'Checksum OK.'

    & $exe -install
    if ($LASTEXITCODE -ne 0) { throw "The install didn't finish (exit code $LASTEXITCODE)." }
}
finally {
    Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
}
