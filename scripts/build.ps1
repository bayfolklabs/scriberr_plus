# Build Scriberr on Windows: React frontend -> embedded into the Go binary -> scriberr.exe
# PowerShell port of `make build` / build.sh. Run from anywhere:
#   powershell -ExecutionPolicy Bypass -File scripts\build.ps1 [-SkipFrontend]
param(
    [switch]$SkipFrontend
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# Pick up tools installed since this shell started (winget updates PATH in the registry only)
$env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')

if (-not $SkipFrontend) {
    Write-Host '==> Building React frontend' -ForegroundColor Cyan
    Push-Location web\frontend
    try {
        if (-not (Test-Path node_modules)) { npm ci --no-audit --no-fund; if ($LASTEXITCODE) { throw 'npm ci failed' } }
        npm run build; if ($LASTEXITCODE) { throw 'frontend build failed' }
    } finally { Pop-Location }

    Write-Host '==> Copying frontend into internal\web\dist (go:embed)' -ForegroundColor Cyan
    # Remove first: Copy-Item into an existing dir would nest dist\dist
    if (Test-Path internal\web\dist) { Remove-Item internal\web\dist -Recurse -Force }
    Copy-Item web\frontend\dist internal\web\dist -Recurse
} elseif (-not (Test-Path internal\web\dist\index.html)) {
    throw 'internal\web\dist is missing; run without -SkipFrontend first'
}

Write-Host '==> Building scriberr.exe' -ForegroundColor Cyan
$env:CGO_ENABLED = '0'
$commit = (git rev-parse --short HEAD 2>$null)
if (-not $commit) { $commit = 'none' }
$date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
go build -ldflags "-X main.version=win-dev -X main.commit=$commit -X main.date=$date" -o scriberr.exe .\cmd\server
if ($LASTEXITCODE) { throw 'go build failed' }

Write-Host "==> Done. Start it from $root with:  .\scriberr.exe   (then open http://localhost:8080)" -ForegroundColor Green
