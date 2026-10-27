# scripts/build_release.ps1
# ==============================================================================
# OmniCoder-ICX - Cross-Platform Zero-CGo Release Packaging Pipeline
# Compiles stripped binaries (-ldflags="-s -w" -trimpath) for windows/amd64,
# linux/amd64, and darwin/arm64, runs smoke verification, and emits SHA256SUMS
# + RELEASE_MANIFEST.json.
# ==============================================================================

[CmdletBinding()]
param(
    [string]$Version = "1.0.0",
    [string]$OutputDir = "release",
    [switch]$SkipArchives = $false
)

$ErrorActionPreference = "Stop"

Write-Host "========================================================================" -ForegroundColor Cyan
Write-Host "   OMNICODER-ICX v$Version - ZERO-CGO RELEASE PACKAGING PIPELINE" -ForegroundColor White
Write-Host "   Targets: windows/amd64, linux/amd64, darwin/arm64 (CGO_ENABLED=0)" -ForegroundColor DarkCyan
Write-Host "========================================================================" -ForegroundColor Cyan

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
$ProjectRoot = Split-Path -Parent $ScriptDir
Set-Location $ProjectRoot

Write-Host "`n[1/5] Verifying Go toolchain..." -ForegroundColor Yellow
$goVersion = go version
Write-Host "      Detected: $goVersion" -ForegroundColor Gray

if (Test-Path $OutputDir) {
    Remove-Item -Recurse -Force $OutputDir
}
New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null
New-Item -ItemType Directory -Path "bin" -Force | Out-Null

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; BinName = "omnicoder.exe";        Archive = "omnicoder-icx-v$Version-windows-amd64.zip" },
    @{ GOOS = "linux";   GOARCH = "amd64"; BinName = "omnicoder-linux-amd64"; Archive = "omnicoder-icx-v$Version-linux-amd64.tar.gz" },
    @{ GOOS = "darwin";  GOARCH = "arm64"; BinName = "omnicoder-darwin-arm64"; Archive = "omnicoder-icx-v$Version-darwin-arm64.tar.gz" }
)

$artifacts = @()
$sumsLines = @()

Write-Host "`n[2/5] Compiling stripped zero-CGo binaries (-ldflags='-s -w' -trimpath)..." -ForegroundColor Yellow

foreach ($t in $targets) {
    $env:CGO_ENABLED = "0"
    $env:GOOS = $t.GOOS
    $env:GOARCH = $t.GOARCH

    $outPath = Join-Path $OutputDir $t.BinName
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    go build -ldflags="-s -w" -trimpath -o $outPath ./cmd/omnicoder
    if ($LASTEXITCODE -ne 0) {
        throw "Go build failed for $($t.GOOS)/$($t.GOARCH)"
    }
    $sw.Stop()

    $item = Get-Item $outPath
    $sizeMB = [math]::Round($item.Length / 1MB, 2)
    $sha256 = (Get-FileHash -Path $outPath -Algorithm SHA256).Hash.ToLower()
    $sumsLines += "$sha256  $($t.BinName)"

    Write-Host "      [OK] $($t.GOOS)/$($t.GOARCH) -> $($t.BinName) ($sizeMB MB in $($sw.ElapsedMilliseconds) ms)" -ForegroundColor Green

    if ($t.GOOS -eq "windows") {
        Copy-Item $outPath "bin\omnicoder.exe" -Force
        Copy-Item $outPath "omnicoder.exe" -Force
    }

    if (-not $SkipArchives) {
        $archivePath = Join-Path $OutputDir $t.Archive
        if ($t.Archive.EndsWith(".zip")) {
            Compress-Archive -Path $outPath, "README.md", "LICENSE", "NOTICE", "CHANGELOG.md" -DestinationPath $archivePath -Force
        } else {
            tar -czf $archivePath -C $OutputDir $t.BinName -C $ProjectRoot README.md LICENSE NOTICE CHANGELOG.md
        }
        $archHash = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()
        $sumsLines += "$archHash  $($t.Archive)"
    }

    $artifacts += [PSCustomObject]@{
        os          = $t.GOOS
        arch        = $t.GOARCH
        binary      = $t.BinName
        archive     = $t.Archive
        size_bytes  = $item.Length
        size_mb     = $sizeMB
        sha256      = $sha256
    }
}

# Restore host env
$env:GOOS = "windows"
$env:GOARCH = "amd64"

Write-Host "`n[3/5] Running host binary smoke verification..." -ForegroundColor Yellow
$winBin = Join-Path $OutputDir "omnicoder.exe"
$verOut = (& $winBin version | Out-String).Trim()
if ($LASTEXITCODE -ne 0) {
    throw "Windows binary smoke check failed with exit code $LASTEXITCODE"
}
Write-Host "      [OK] Smoke check passed: $verOut" -ForegroundColor Green

Write-Host "`n[4/5] Writing SHA256SUMS and RELEASE_MANIFEST.json..." -ForegroundColor Yellow
$sumsPath = Join-Path $OutputDir "SHA256SUMS"
$sumsLines -join "`n" | Out-File -FilePath $sumsPath -Encoding ascii -Force

$manifest = [PSCustomObject]@{
    product      = "OmniCoder-ICX"
    module       = "github.com/caleralabs/omnicoder-icx"
    version      = $Version
    license      = "Apache-2.0"
    build_date   = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    cgo_enabled  = "0"
    artifacts    = $artifacts
}
$manifestPath = Join-Path $OutputDir "RELEASE_MANIFEST.json"
$manifest | ConvertTo-Json -Depth 5 | Out-File -FilePath $manifestPath -Encoding utf8 -Force

Write-Host "`n[5/5] Release packaging complete!" -ForegroundColor Green
Write-Host "      Output Directory: $OutputDir" -ForegroundColor Cyan
Write-Host "      Checksums:        $sumsPath" -ForegroundColor Gray
Write-Host "      Manifest:         $manifestPath" -ForegroundColor Gray
Write-Host "========================================================================" -ForegroundColor Cyan
