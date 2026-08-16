<#
.SYNOPSIS
    Installs FORGE for the current user.

.DESCRIPTION
    irm https://raw.githubusercontent.com/VEER-TARGARYEN/forge/main/install.ps1 | iex

    Downloads the installer for your architecture from the latest GitHub
    release, checks it against the published SHA256SUMS, and runs it.
    Everything lands under %LOCALAPPDATA%: no administrator rights, no UAC
    prompt, and nothing written outside your own profile.

    Piping a script into a shell is a thing to be suspicious of. This one is
    short on purpose so it can be read first:

        irm https://raw.githubusercontent.com/VEER-TARGARYEN/forge/main/install.ps1 -OutFile install.ps1
        notepad install.ps1
        .\install.ps1

.PARAMETER Version
    Install a specific tag, e.g. v1.0.0. Defaults to the latest release.

.PARAMETER Dir
    Install to a specific directory instead of %LOCALAPPDATA%\Programs\FORGE.

.PARAMETER SkipVerify
    Skip checksum verification. Not recommended.
#>

[CmdletBinding()]
param(
    [string] $Version    = $env:FORGE_VERSION,
    [string] $Dir        = $env:FORGE_DIR,
    [switch] $SkipVerify,
    [switch] $NoLaunch
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'   # the progress bar makes IWR crawl

$Repo = 'VEER-TARGARYEN/forge'
$App  = 'FORGE'

function Die($msg) {
    Write-Host ''
    Write-Host "error: $msg" -ForegroundColor Red
    exit 1
}

# TLS 1.2 is not the default on Windows PowerShell 5.1, and GitHub requires it.
try {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { }

# ---- architecture -----------------------------------------------------------

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    'x86'   {
        # A 32-bit shell on a 64-bit machine still wants the 64-bit build.
        if ($env:PROCESSOR_ARCHITEW6432 -eq 'AMD64') { 'amd64' }
        elseif ($env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { 'arm64' }
        else { Die 'FORGE requires 64-bit Windows.' }
    }
    default { Die "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

# ---- resolve the release ----------------------------------------------------

if (-not $Version) {
    Write-Host 'Finding the latest release...'
    try {
        $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
                                 -Headers @{ 'User-Agent' = 'forge-installer' }
        $Version = $rel.tag_name
    } catch {
        Die "could not determine the latest release: $($_.Exception.Message)`nPass -Version v1.0.0 to pick one explicitly."
    }
}

$asset = "$App-setup-windows-$arch.exe"
$base  = "https://github.com/$Repo/releases/download/$Version"

Write-Host "$App $Version  (windows/$arch)"

# ---- download ---------------------------------------------------------------

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("forge-" + [System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
    $exe = Join-Path $tmp $asset

    Write-Host "Downloading $asset..."
    try {
        Invoke-WebRequest -Uri "$base/$asset" -OutFile $exe -UseBasicParsing `
                          -Headers @{ 'User-Agent' = 'forge-installer' }
    } catch {
        Die "no build for windows/$arch in $Version.`nSee https://github.com/$Repo/releases/$Version for what is available."
    }

    # ---- verify -------------------------------------------------------------

    if ($SkipVerify) {
        Write-Host 'Skipping checksum verification.' -ForegroundColor Yellow
    } else {
        $sums = Join-Path $tmp 'SHA256SUMS'
        $havesums = $true
        try {
            Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sums -UseBasicParsing `
                              -Headers @{ 'User-Agent' = 'forge-installer' }
        } catch { $havesums = $false }

        if (-not $havesums) {
            Write-Host "warning: SHA256SUMS not published for $Version; continuing unverified." -ForegroundColor Yellow
        } else {
            $line = Get-Content $sums | Where-Object { $_ -match "\s$([regex]::Escape($asset))$" } | Select-Object -First 1
            if (-not $line) {
                Write-Host "warning: $asset is not listed in SHA256SUMS; continuing unverified." -ForegroundColor Yellow
            } else {
                $expected = ($line -split '\s+')[0]
                $actual   = (Get-FileHash -Path $exe -Algorithm SHA256).Hash.ToLower()
                if ($actual -ne $expected.ToLower()) {
                    Die "checksum mismatch for $asset.`n  expected $expected`n  got      $actual`nDo not run it. Please open an issue at https://github.com/$Repo/issues"
                }
                Write-Host 'Checksum verified.'
            }
        }
    }

    # ---- install ------------------------------------------------------------

    Write-Host ''
    $args = @()
    if ($Dir)       { $args += @('-dir', $Dir) }
    if ($NoLaunch)  { $args += '-launch=false' }
    & $exe @args
    if ($LASTEXITCODE -ne 0) { Die "the installer exited with code $LASTEXITCODE" }

} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

Write-Host ''
Write-Host 'Get started (in a NEW terminal, so PATH is picked up):'
Write-Host ''
Write-Host '    forge init          # write a starter config'
Write-Host '    forge doctor        # check which providers are reachable'
Write-Host '    forge app           # open the desktop interface'
Write-Host ''
