[CmdletBinding()]
param(
    [string]$DistroName = "Ubuntu-22.04",
    [string]$NodeId = "gpu-node-1",
    [string]$Site = "default",
    [string]$ControlPlaneUrl = "",
    [switch]$Install
)

$ErrorActionPreference = "Stop"

Write-Host "Checking WSL2 and NVIDIA prerequisites for $NodeId..." -ForegroundColor Cyan

$wsl = Get-Command wsl.exe -ErrorAction SilentlyContinue
if (-not $wsl) {
    throw "wsl.exe was not found. Enable WSL2 first, or rerun this script with -Install from an elevated PowerShell."
}

if ($Install) {
    Write-Host "Updating WSL kernel..."
    wsl.exe --update
}

$status = (wsl.exe --status 2>&1 | Out-String)
if ($LASTEXITCODE -ne 0) {
    throw "WSL2 is not ready. Install it with: wsl --install --no-distribution"
}
Write-Host $status

$distros = (wsl.exe --list --quiet 2>$null) -replace "`0", "" | Where-Object { $_.Trim() }
if ($distros -notcontains $DistroName) {
    if (-not $Install) {
        throw "$DistroName is not installed. Rerun with -Install or install it manually using: wsl --install -d $DistroName"
    }
    Write-Host "Installing $DistroName..."
    wsl.exe --install -d $DistroName
    throw "The distro was installed. Reboot if Windows requests it, then rerun this script without -Install."
}

$nvidia = Get-Command nvidia-smi.exe -ErrorAction SilentlyContinue
if (-not $nvidia) {
    Write-Warning "nvidia-smi.exe was not found on Windows. Install/update the NVIDIA driver with WSL2 support."
} else {
    & $nvidia.Source --query-gpu=name,memory.total,driver_version --format=csv,noheader
    if ($LASTEXITCODE -ne 0) {
        throw "nvidia-smi failed. Check the Windows NVIDIA driver before starting the agent."
    }
}

$docker = Get-Command docker.exe -ErrorAction SilentlyContinue
if (-not $docker) {
    throw "docker.exe was not found. Install Docker Desktop and enable the WSL2 backend."
}

Write-Host "Docker version:" -ForegroundColor DarkGray
docker.exe version --format '{{.Server.Version}}'
Write-Host "Docker GPU probe:" -ForegroundColor DarkGray
docker.exe run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi
if ($LASTEXITCODE -ne 0) {
    throw "Docker cannot access the GPU. In Docker Desktop, enable the WSL2 engine and GPU support."
}

Write-Host "Agent settings:" -ForegroundColor Cyan
Write-Host "  NODE_ID=$NodeId"
Write-Host "  SITE=$Site"
Write-Host "  CONTROL_PLANE_URL=$ControlPlaneUrl"
Write-Host "Create deploy/compose/.env.agent on this host with these values, then run:"
Write-Host "  docker compose --env-file deploy/compose/.env.agent -f deploy/compose/docker-compose.agent.yml up -d --build"
Write-Host "Use a different NODE_ID and SITE for every physical host."
