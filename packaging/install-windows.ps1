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
# Normalize-Sha lowercases a hex digest and requires exactly 64 hex characters,
# so a truncated or malformed checksum record can never be compared as if it
# were a valid digest.
function Normalize-Sha($value) {
    $sha = "$value".Trim().ToLower()
    if ($sha -notmatch '^[0-9a-f]{64}$') {
        throw "install-windows.ps1: malformed sha256 digest: '$value'"
    }
    return $sha
}

# Assert-Tag rejects anything that is not a plain release identifier, so a
# value like '../../evil' cannot reach the download URL as path traversal.
function Assert-Tag($value) {
    if ($value -notmatch '^RELEASE\.[A-Za-z0-9._-]+$') {
        throw "install-windows.ps1: unexpected release tag '$value'"
    }
}

# A pinned BUCKIT_VERSION leaves $pointerSha empty: the pointer only ever
# describes the latest release, so it cannot vouch for an arbitrary pin.
$pointerSha = ''
$tag = [Environment]::GetEnvironmentVariable('BUCKIT_VERSION')
if ([string]::IsNullOrEmpty($tag)) {
    $pointerUrl = "$PagesBase/server/buckit/release/windows-amd64/buckit.sha256sum"
    try {
        $pointer = (Fetch-String $pointerUrl).Trim()
    } catch {
        throw "install-windows.ps1: could not fetch release pointer at $pointerUrl"
    }
    $fields = ($pointer -split '\r?\n')[0] -split '\s+'
    $name = $fields[1]
    if ($name -notlike 'buckit.exe.*') {
        throw "install-windows.ps1: unexpected release pointer payload: $pointer"
    }
    $tag = $name -replace '^buckit\.exe\.', ''
    $pointerSha = Normalize-Sha $fields[0]
}
Assert-Tag $tag
Write-Host "==> release: $tag"

$asset = "buckit-windows-amd64.exe.$tag"
$downloadUrl = "$ReleaseBase/$tag/$asset"

$dlDir = Get-EnvOrDefault 'BUCKIT_DOWNLOAD_DIR' (Get-Location).Path
New-Item -ItemType Directory -Force -Path $dlDir | Out-Null
$exeFile = Join-Path $dlDir 'buckit.exe'

# Refuse to run when the destination is a directory. Move-Item would move the
# temp file inside it and the script would report success while leaving nothing
# runnable at the path it prints.
if (Test-Path -LiteralPath $exeFile -PathType Container) {
    throw "install-windows.ps1: $exeFile is a directory - remove it or set BUCKIT_DOWNLOAD_DIR"
}

# Download to a temporary sibling and only move it into the predictable path
# after the checksum verifies, so a failed download cannot clobber an existing
# good executable or leave a partial/unverified file.
$tmpFile = Join-Path $dlDir ".buckit.$([System.IO.Path]::GetRandomFileName()).tmp"
try {
    Write-Host "==> downloading $asset"
    Invoke-WebRequest -UseBasicParsing -Uri $downloadUrl -OutFile $tmpFile

    Write-Host "==> fetching published checksum"
    $releaseSha = Normalize-Sha (((Fetch-String "$downloadUrl.sha256sum").Trim() -split '\s+')[0])

    # The binary and the checksum beside it come from the same origin, so that
    # digest alone only proves the download was not corrupted in transit. The
    # gh-pages pointer publishes the same digest from a separate origin;
    # when it is available, require the two to agree before trusting either.
    if ($pointerSha) {
        if ($pointerSha -ne $releaseSha) {
            throw "install-windows.ps1: published digests disagree (pages $pointerSha, release $releaseSha) - refusing to continue"
        }
        $wantSha = $pointerSha
        Write-Host "==> sha256 cross-checked against the release pointer"
    } else {
        $wantSha = $releaseSha
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
Write-Host "Run it from here:"
Write-Host "  & `"$exeFile`""
Write-Host ""
Write-Host "(Add its folder to your PATH to call 'buckit' from anywhere.)"
Write-Host ""
