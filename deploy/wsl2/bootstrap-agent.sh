#!/usr/bin/env bash
set -euo pipefail

NODE_ID="${NODE_ID:-gpu-node-1}"
SITE="${SITE:-default}"
CONTROL_PLANE_URL="${CONTROL_PLANE_URL:-}"

echo "Checking WSL2/Docker GPU prerequisites for ${NODE_ID}..."
command -v nvidia-smi >/dev/null || {
  echo "nvidia-smi is unavailable. Install a Windows NVIDIA driver with WSL2 support." >&2
  exit 1
}
command -v docker >/dev/null || {
  echo "docker is unavailable inside WSL2. Enable Docker Desktop WSL integration." >&2
  exit 1
}

nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader
docker version --format '{{.Server.Version}}'
echo "This checks NVIDIA management access only, not CUDA model execution compatibility."
docker run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi

cat <<EOF

Prerequisites passed.
This starts inventory collection only. Actual inference requires
docker-compose.distributed-agent.yml or docker-compose.petals.yml.
Start the node agent from the repository root:

  docker compose --env-file deploy/compose/.env.agent -f deploy/compose/docker-compose.agent.yml up -d --build

Override these variables for each host:
  NODE_ID=${NODE_ID}
  SITE=${SITE}
  CONTROL_PLANE_URL=${CONTROL_PLANE_URL}

Use a unique NODE_ID per physical host; SITE can be shared by nearby hosts.
Read docs/多机接入与验收清单.md and collect an edge-agent --inspect report
before enabling scheduling.
EOF
