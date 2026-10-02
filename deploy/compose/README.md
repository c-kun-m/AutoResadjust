# Compose 部署骨架

这套 Compose 文件用于第一版的本地联调和三台 Windows + WSL2 + Docker Desktop GPU 主机部署。每一台物理 GPU 主机只运行一份 `edge-agent`；控制面和可选基础设施运行在管理主机。

## 控制面

复制 `.env.example` 为 `.env`，替换密码后，在仓库根目录启动控制面：

```powershell
docker compose --env-file deploy/compose/.env -f deploy/compose/docker-compose.yml up -d --build control-plane
```

默认只绑定 `127.0.0.1:8080`，健康检查地址为 `http://127.0.0.1:8080/healthz`。设置 `CONTROL_PLANE_TOKEN` 后，API 和 agent 心跳要求 Bearer token；控制面使用内存状态和原子文件快照。PostgreSQL、NATS、MinIO、Keycloak 尚非业务依赖。`observability` profile 的 Prometheus、Grafana、cAdvisor、Loki 和 Alloy 已接入指标、日志及告警。

如果远程 GPU 主机需要访问控制面，应把 `CONTROL_PLANE_BIND` 设置为管理主机的明确 LAN 地址（或 `0.0.0.0`），同时先配置 TLS、身份认证和防火墙策略；不要把未经保护的开发端口直接暴露到公网。

## 可选基础设施

按需启动基础设施 profile，避免开发时占用不必要的资源：

```powershell
docker compose --env-file deploy/compose/.env -f deploy/compose/docker-compose.yml --profile infra up -d
docker compose --env-file deploy/compose/.env -f deploy/compose/docker-compose.yml --profile identity up -d keycloak
docker compose --env-file deploy/compose/.env -f deploy/compose/docker-compose.yml --profile observability up -d
```

所有端口默认只绑定本机。NATS 以 `-m 8222` 开启监控端口，客户端端口为 4222；Keycloak 25 使用 `KC_BOOTSTRAP_ADMIN_USERNAME` 和 `KC_BOOTSTRAP_ADMIN_PASSWORD` 变量。

## 每台 GPU 主机的 agent

在每台 Windows 主机上运行 `deploy/wsl2/bootstrap-agent.ps1`，或在 WSL2 中运行对应的 `bootstrap-agent.sh`，确认 `nvidia-smi` 和 Docker GPU 探针通过。然后为该物理主机创建独立环境文件，例如 `deploy/compose/.env.agent-a`：

```dotenv
NODE_ID=dc-a-node-01
NODE_NAME=dc-a-node-01
SITE=dc-a
CONTROL_PLANE_URL=http://192.168.10.10:8080
```

在该主机上只启动这一份 agent Compose：

```powershell
docker compose --env-file deploy/compose/.env.agent-a -f deploy/compose/docker-compose.agent.yml up -d --build
```

对第二、第三台物理主机分别使用不同的 `NODE_ID`、`SITE` 和环境文件。不要在一台机器上启动三份 agent 来虚构三个节点；agent 上报的是本机实际 `nvidia-smi` 资源。

## 运行边界

- Windows 主机使用 NVIDIA Windows 驱动 + WSL2 + Docker Desktop WSL2 backend；agent 在 Linux CUDA 容器内运行。
- 以上 `docker-compose.agent.yml` 是资源采集器。实际推理用 `docker-compose.distributed-agent.yml`（RPC/本机内存）或 `docker-compose.petals.yml`（私有固定连续块）；详见仓库 README。
- `gpus: all` 授予 Agent 发现 GPU 的能力，实际推理子进程的 CUDA 可见集合由执行器按调度结果设置。
- Compose 是开发和首版验收编排方式。生产环境保持服务边界，后续再接入 PostgreSQL/NATS，并将执行器适配到 Kubernetes + Kueue。

## 日志及故障验收

管理端启动 `observability` profile 即启用按项目隔离的集中日志。远端 Agent 可叠加 `docker-compose.logs.yml`；Loki 地址、采集器监控、7 天保留和回放窗口、升级前数据备份要求见 [日志与验收指南](../../docs/集中日志与协作推理验收.md)。没有多物理机时，隔离容器内的网络故障测试只验证协议行为。
