param(
  [string]$Distro = "Ubuntu"
)

$ErrorActionPreference = "Stop"
Write-Host "Checking WSL2 + Docker GPU prerequisites for $Distro"

wsl.exe --status
wsl.exe -d $Distro -- bash -lc "command -v nvidia-smi && nvidia-smi --query-gpu=name,memory.total --format=csv,noheader"
docker version
docker run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi

Write-Host "Prerequisite check passed. Run the edge agent only after the control plane is reachable."
