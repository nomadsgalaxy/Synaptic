# Synaptic — one-line starter for Windows.
#
# Brings up the Docker stack (synaptic-disorder-core + dashboard + ollama),
# waits for the dashboard to respond, and opens it in the default browser.
#
# Usage from the repo root:
#     PowerShell -ExecutionPolicy Bypass -File tools\install\start.ps1
#
# Or just double-click `start.bat` in the repo root (companion file).
[CmdletBinding()]
param(
    [string]$ComposeFile = "docker-compose.yml",
    # SD Core serves the dashboard on 9911 (matches docker-compose.yml's
    # ports binding).  The legacy static-only fallback `serve.py` uses 8765;
    # this Docker-based launcher must use 9911.
    [int]$Port = 9911,
    [switch]$NoOpen
)

$ErrorActionPreference = "Stop"

# 1. Sanity-check Docker.
$docker = Get-Command docker -ErrorAction SilentlyContinue
if (-not $docker) {
    Write-Host "Docker isn't on PATH. Install Docker Desktop:" -ForegroundColor Yellow
    Write-Host "    https://www.docker.com/products/docker-desktop/" -ForegroundColor Yellow
    exit 1
}

# 2. Bring up the stack.
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location (Split-Path -Parent $repoRoot)
try {
    Write-Host "starting Synaptic Docker stack..." -ForegroundColor Cyan
    docker compose -f $ComposeFile up -d 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) {
        Write-Host "docker compose up failed -- inspect output above." -ForegroundColor Red
        exit $LASTEXITCODE
    }

    # 3. Poll the dashboard until it responds (up to 30s).
    $url = "http://localhost:$Port/index.html"
    Write-Host "waiting for dashboard at $url ..." -ForegroundColor Cyan
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 2
            if ($r.StatusCode -eq 200) { break }
        } catch {}
        Start-Sleep -Milliseconds 500
    }

    Write-Host ""
    Write-Host "Synaptic is up." -ForegroundColor Green
    Write-Host "  Dashboard:  $url"
    Write-Host "  SD Core:    http://localhost:9911/healthz"
    Write-Host "  Ollama:     http://localhost:11434"
    Write-Host ""
    Write-Host "Stop with: docker compose down" -ForegroundColor Cyan

    if (-not $NoOpen) {
        Start-Process $url
    }
} finally {
    Pop-Location
}
