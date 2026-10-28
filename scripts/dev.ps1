# Dev loop on Windows: Go backend on :8080 + Vite dev server (hot reload) on :5173.
# PowerShell port of `make dev`. Open http://localhost:5173 (Vite proxies /api to :8080).
#   powershell -ExecutionPolicy Bypass -File scripts\dev.ps1
# Ctrl+C stops both. Backend Go changes need a restart of this script.

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
$env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')

# go:embed needs internal\web\dist to exist; in dev the UI comes from Vite, so a placeholder is enough
if (-not (Test-Path internal\web\dist\index.html)) {
    New-Item -ItemType Directory -Force internal\web\dist | Out-Null
    '<!-- Placeholder for development: use http://localhost:5173 -->' | Out-File internal\web\dist\index.html -Encoding utf8
}

if (-not (Test-Path web\frontend\node_modules)) {
    Push-Location web\frontend; try { npm ci --no-audit --no-fund } finally { Pop-Location }
}

Write-Host '==> Building backend' -ForegroundColor Cyan
New-Item -ItemType Directory -Force tmp | Out-Null
go build -o tmp\scriberr-dev.exe .\cmd\server
if ($LASTEXITCODE) { throw 'go build failed' }

Write-Host '==> Starting backend on http://localhost:8080' -ForegroundColor Cyan
$backend = Start-Process -FilePath (Join-Path $root 'tmp\scriberr-dev.exe') -WorkingDirectory $root -NoNewWindow -PassThru

try {
    Write-Host '==> Starting Vite on http://localhost:5173' -ForegroundColor Cyan
    Push-Location web\frontend
    npm run dev
} finally {
    Pop-Location
    if ($backend -and -not $backend.HasExited) {
        Write-Host '==> Stopping backend' -ForegroundColor Cyan
        taskkill /T /F /PID $backend.Id | Out-Null
    }
}
