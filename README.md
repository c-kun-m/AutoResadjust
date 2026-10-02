# 算力协作平台

目标是把多台普通 NVIDIA 显卡连接起来，让一个模型的权重和计算分布到多个节点。当前支持 llama.cpp RPC、私有 Petals 连续模型块，以及单机 GPU + 内存；控制面负责整组调度，Agent 启停实际推理进程，并提供每个服务的状态和流量监控。

## 已实现

- **分布式模型部署**：同一可信网络组内选择 2–8 个节点，每节点一张 GPU。按空闲显存扣除运行和 KV 缓存预算后规划；所有节点使用同一个引擎版本；整组原子预留、节点独占，拒绝重复 GPU UUID/RPC 地址。
- **执行和生命周期**：Agent 启动 `ggml-rpc-server`；全部工作节点就绪后，协调节点启动 `llama-server`。模型加载成功才开放统一聊天接口，支持 SSE 流式输出。停止先排空推理，全部进程确认终止才释放资源。
- **服务监控**：控制面、Web 控制台、Agent API、控制连接、每个 GPU RPC 工作进程、模型进程、推理入口，以及配置的基础设施健康和网络流量。未采集到的指标明确标记，不生成模拟值。
- **监控栈**：Prometheus 抓取与 15 天保留，Grafana 20 个面板，cAdvisor 容器指标，12 条告警规则。Alloy 按项目采集日志，Loki 持久化保留 7 天；按服务/节点/部署查询，工作进程最近片段仍可在控制台查看。
- **统一聊天入口**：相同模型版本与后端的实例池，统一 `/v1/models`、`/v1/chat/completions`；每部署共享并发、排队、首 token/停滞/总超时和取消，旧部署入口也执行相同保护。控制台“聊天入口”显示真实请求进度。
- **恢复**：节点、任务、部署、幂等键和资源预留原子保存到本地 JSON；重启后保留预留并等待 Agent 重新确认。单控制面，不是数据库集群或高可用方案。
- **节点管理**：真实 `nvidia-smi` 心跳、显存、利用率、禁用/启用/排空。新节点默认禁止调度。保留旧版单节点调度 API，它只产生调度记录，不启动模型。

实现和部署细节见 [分布式推理与服务监控](docs/分布式推理与服务监控.md)。原来的设计文档和第一版实施计划保留作背景，以这份 README 和新指南描述的代码能力为准。

## 协作推理升级进度

已加入 `llama_local` 单机 GPU + 主机内存部署、不可变 GGUF 模型目录与 SHA-256 校验、主机 CPU/RAM 采集和预留。原有 `llama_rpc` 和旧部署 API 继续使用；新节点必须上报后端能力才能参与本机内存部署。模型就绪现在要求一次真实的端到端短回复预热，运行中的健康检查失败不会被误判成加载超时。

控制台“模型部署”可选择执行方式、登记模型和预览 GPU/RAM 分配；“服务监控”显示主机实测内存、CPU 与部署预留。“网络与分组”支持有界的双向测量、链路矩阵和手动网络组；配置了策略的组必须有满足阈值的新鲜链路数据才能调度。状态文件升级为 v5，自动读取 v1–v4；降级前需停止新增类型部署并恢复升级前备份。

这些功能已通过自动化与浏览器流程测试，并完成 RTX 5060 上真实 135M 模型的加载、预热、聊天、流式取消和停止验证。私有 Petals 已接入连续块调度、离线模型封装、网关、租约和各服务监控，实际单卡双进程推理、取消、断块、冻结无响应和租约终止测试通过；部署步骤见 [私有 Petals](python/petals_backend/README.md)。集中日志已接入并实测，性能对照及隔离故障实验见 [日志与验收指南](docs/集中日志与协作推理验收.md)。**尚未完成真实 13B 模型和多机 GPU 性能验收**，具体状态与证据见 [协作推理开发进度](docs/协作推理开发进度.md)。

## 本地启动控制面和监控

需要 Go 1.22+、Node.js 和 Docker Compose。先在项目根目录执行：

```powershell
cd web-console
npm ci
npm run build
cd ..
docker compose -f deploy/compose/docker-compose.yml --profile observability up -d --build control-plane prometheus grafana loki cadvisor alloy
```

默认入口：控制台 `http://127.0.0.1:8080`，Prometheus `http://127.0.0.1:9090`，Grafana `http://127.0.0.1:3000/d/compute-services`。Grafana 本地初始登录为 admin/admin。控制面状态保存在 `control-plane-data` 卷。

无 Docker 时，可运行 `go run ./cmd/control-plane --web-dir web-console/dist`；基础设施的容器流量需要额外启动 cAdvisor 并配置 `--services-config`。

2026-09-23 本次检查使用独立项目 `resource-adjust-observe-check` 和 `deploy/compose/docker-compose.review.yml`，入口分别是 **18090 / 19090 / 13000**。该控制面接入了本机真实 RTX 5060 的采集 Agent，默认禁用调度，未运行模型。它是独立验证实例，不共享此前 8080 实例的状态。

## 接入多台 GPU

先按 [多机接入与验收清单](docs/多机接入与验收清单.md) 运行只读 `edge-agent --inspect`，核对主机与实际容器内的 GPU/内存报告。

1. 每台物理机器安装 NVIDIA 驱动、WSL2/Docker GPU 支持，确认容器中的 `nvidia-smi` 可用。
2. 管理端复制 `deploy/compose/.env.example`，配置共享令牌和私网监听地址。所有 Agent 使用相同令牌。
3. 每台机器复制 `.env.distributed-agent.example`，填写唯一节点 ID、可互通的私网 IP、控制面地址和模型目录。每台运行一份 `docker-compose.distributed-agent.yml`，不要将同一张卡注册多次。
4. 在“算力节点”确认真实心跳并启用；在“模型部署”填写 GGUF 文件和预算，预览后创建。GGUF 必须事先放在协调节点的模型目录中。
5. 在“服务监控”检查每个进程的状态、请求和流量；部署进入 ready 后测试聊天。

## 验证与边界

```powershell
go test ./...
go vet ./...
# 有 C 编译器的 Linux 环境中运行
go test -race ./...
```

本轮已通过 Go 测试、Linux race 检测、前端构建、控制面容器启动、Prometheus 抓取与规则校验、Grafana 面板加载，以及浏览器操作检查。测试覆盖整组预留、并发冲突、幂等提交、失联保留资源、停止确认、持久化恢复、真实 TCP/HTTP 字节计数和 SSE 转发。执行器测试使用测试子进程，不代表真实模型性能。

2026-10-01 已构建固定 llama.cpp 版本的 CUDA 12.8.1 / SM120 镜像，并在 RTX 5060 上以 15 个 GPU 层运行真实 SmolLM2 135M Q8 模型；控制面、Agent、模型预热与请求转发、停止释放完成验证。**尚未完成真实多机 GPU 推理验收**；仍需要至少两台真实机器和 13B 权重进行容量、首 token 延迟及持续生成吞吐测试。小模型冒烟结果不代表大模型性能。

上游 [llama.cpp RPC](https://github.com/ggml-org/llama.cpp/blob/e6ab7c1a41054a888ada952eab4c886444c2f5ad/tools/rpc/README.md) 仍处于实验阶段，原始 RPC 没有应用层认证，必须限制在可信私网。多卡主要扩大可容纳模型的规模；普通网络不保证更快。首版不覆盖训练、AMD/Apple GPU、跨公网协同、自动模型下载、自动迁移或多租户隔离。
