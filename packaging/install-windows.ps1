<#
.SYNOPSIS
  Windows installer helper for buckit.

.DESCRIPTION
  Downloads the Windows (x86_64) buckit executable for the latest stable
  release to a predictable filename (buckit.exe), verifies its published
  SHA-256 checksum, and prints how to put it on your PATH. It does NOT install
  it for you.

.EXAMPLE
  irm https://buckit-io.github.io/buckit/install-windows.ps1 | iex

.NOTES
  Environment overrides:
    BUCKIT_PAGES_BASE     gh-pages base URL
                          (default: https://buckit-io.github.io/buckit)
    BUCKIT_RELEASE_BASE   release download base
                          (default: https://github.com/buckit-io/buckit/releases/download)
    BUCKIT_VERSION        pin a release tag (e.g. RELEASE.2026-05-11T17-20-40Z)
                          instead of resolving the latest stable release
    BUCKIT_DOWNLOAD_DIR   directory to download the executable into
                          (default: the current directory)
#>

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

function Get-EnvOrDefault($name, $default) {
    $value = [Environment]::GetEnvironmentVariable($name)
    if ([string]::IsNullOrEmpty($value)) { return $default }
    return $value
}

$PagesBase = Get-EnvOrDefault 'BUCKIT_PAGES_BASE' 'https://buckit-io.github.io/buckit'
$ReleaseBase = Get-EnvOrDefault 'BUCKIT_RELEASE_BASE' 'https://github.com/buckit-io/buckit/releases/download'

function Fetch-String($url) {
    return (Invoke-WebRequest -UseBasicParsing -Uri $url).Content
}

# Require Windows. $IsWindows is an automatic variable on PowerShell Core 6+
# (where this script may run cross-platform); it is undefined on Windows
# PowerShell 5.1, which only runs on Windows — so treat undefined as Windows.
if (($null -ne $IsWindows) -and (-not $IsWindows)) {
    throw "install-windows.ps1 is for Windows. On Linux use install-linux.sh; on macOS use install-mac.sh"
}

# Resolve the release tag: pinned BUCKIT_VERSION or the latest stable tag from
# the gh-pages pointer (format: "<sha256>  buckit.exe.<tag>").
$tag = [Environment]::GetEnvironmentVariable('BUCKIT_VERSION')
if ([string]::IsNullOrEmpty($tag)) {
    $pointerUrl = "$PagesBase/server/buckit/release/windows-amd64/buckit.sha256sum"
    try {
        $pointer = (Fetch-String $pointerUrl).Trim()
    } catch {
        throw "install-windows.ps1: could not fetch release pointer at $pointerUrl"
    }
    $name = ($pointer -split '\s+')[1]
    $tag = $name -replace '^buckit\.exe\.', ''
}
if ($tag -notlike 'RELEASE.*') {
    throw "install-windows.ps1: unexpected release tag '$tag'"
}
Write-Host "==> release: $tag"

$asset = "buckit-windows-amd64.exe.$tag"
$downloadUrl = "$ReleaseBase/$tag/$asset"

$dlDir = Get-EnvOrDefault 'BUCKIT_DOWNLOAD_DIR' (Get-Location).Path
New-Item -ItemType Directory -Force -Path $dlDir | Out-Null
$exeFile = Join-Path $dlDir 'buckit.exe'

# Download to a temporary sibling and only move it into the predictable path
# after the checksum verifies, so a failed download cannot clobber an existing
# good executable or leave a partial/unverified file.
$tmpFile = Join-Path $dlDir ".buckit.$([System.IO.Path]::GetRandomFileName()).tmp"
try {
    Write-Host "==> downloading $asset"
    Invoke-WebRequest -UseBasicParsing -Uri $downloadUrl -OutFile $tmpFile

    Write-Host "==> fetching published checksum"
    $wantSha = ((Fetch-String "$downloadUrl.sha256sum").Trim() -split '\s+')[0].ToLower()
    if ([string]::IsNullOrEmpty($wantSha)) {
        throw "install-windows.ps1: release checksum is empty"
    }

    $gotSha = (Get-FileHash -Algorithm SHA256 -Path $tmpFile).Hash.ToLower()
    if ($gotSha -ne $wantSha) {
        throw "install-windows.ps1: checksum mismatch (expected $wantSha, got $gotSha) — refusing to continue"
    }
    Write-Host "==> sha256 verified"

    Move-Item -Force -Path $tmpFile -Destination $exeFile
} finally {
    if (Test-Path $tmpFile) { Remove-Item -Force $tmpFile }
}

Write-Host ""
Write-Host "Downloaded and verified:"
Write-Host "  $exeFile"
Write-Host ""
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\buckit'
Write-Host "To install, move it onto your PATH, e.g.:"
Write-Host "  New-Item -ItemType Directory -Force -Path `"$installDir`" | Out-Null"
Write-Host "  Move-Item `"$exeFile`" `"$installDir\buckit.exe`""
Write-Host ""
