# 私有 Petals 兼容性验证

控制面的 `petals` 后端现已接入固定块分配、GPU/RAM/KV 预留、租约、预热、推理入口和服务监控。当前支持标准 Llama safetensors、FP16/INT8/NF4、可信私网。真实验证仍限于一张 RTX 5060 上的两个工作进程；没有完成多物理机或 13B 性能验收。

## 固定输入

| 组件 | 版本 |
| --- | --- |
| 基础镜像 | `pytorch/pytorch:2.8.0-cuda12.8-cudnn9-runtime@sha256:417bd75df6365104c283ea4c1651fb3530d9eb5a4c2fafa51943cff2a94e6385` |
| Petals | `22afba627a7eb4fcfe9418c49472c6a51334b8ac` |
| Hivemind | `213bff98a62accb91f254e2afdccbf1d69ebdea9` |
| py-multiaddr | `e01dbd38f2c0464c0f78b556691d655265018cce` |
| libp2p daemon | `v0.5.0.hivemind1`，SHA-256 `42f8f48e62583b97cdba3c31439c08029fb2b9fc506b5bdd82c46b7cc1d279d8` |
| Transformers | `4.43.1`，保留上游版本断言 |
| bitsandbytes | 显式适配为 `0.48.1`，覆盖上游旧 `0.41.1` 依赖 |

相对于固定基础镜像，新增或改变的 Python 包锁在 `requirements-profile.txt`。源码适配只更新 bitsandbytes 声明，以及将两个 Git 依赖指向已校验提交的本地构建包；安装后执行 `pip check`。没有通过关闭 Transformers 版本断言来掩盖兼容性问题。当前实际验证设备仅为 RTX 5060；其他 GPU 需要重新执行验证。

`PrivateLlama` 使用调用方显式创建的 DHT，避免 CausalLM 构造器隐式创建客户端。只接受明确的引导节点与工作节点白名单，`max_retries=1` 表示一次尝试。固定地址策略拒绝公网、DNS、链路本地元数据地址、UDP 与无端口地址；禁用中继。地址检查不能替代团队 VPN/防火墙，也不是对 DHT 内部其他参与者的网络隔离机制。

## 构建

在项目根目录准备上游源码，保持 checkout 干净。下面只下载官方固定提交与官方发布的 daemon；不下载模型权重：

```powershell
git clone https://github.com/bigscience-workshop/petals.git tmp/petals-source
git -C tmp/petals-source checkout --detach 22afba627a7eb4fcfe9418c49472c6a51334b8ac
git clone https://github.com/learning-at-home/hivemind.git tmp/hivemind-source
git -C tmp/hivemind-source checkout --detach 213bff98a62accb91f254e2afdccbf1d69ebdea9
git clone https://github.com/multiformats/py-multiaddr.git tmp/multiaddr-source
git -C tmp/multiaddr-source checkout --detach e01dbd38f2c0464c0f78b556691d655265018cce
Invoke-WebRequest https://github.com/learning-at-home/go-libp2p-daemon/releases/download/v0.5.0.hivemind1/p2pd-linux-amd64 -OutFile tmp/p2pd-linux-amd64
./scripts/build-petals-compat.ps1
```

脚本校验提交、未修改状态和二进制校验和，再交给 Docker 命名构建上下文。Linux 可直接使用同一 Dockerfile 和 `--build-context petals-source=...`、`hivemind-source`、`multiaddr-source`、`p2pd-cache`（目录内必须有 `p2pd-linux-amd64`），但也应先校验上述固定输入。

## 验证

```powershell
docker run --rm --init --gpus all --network none --shm-size 1g compute-platform-petals-compat:local --fault-test
```

测试完全离线，所有 DHT 地址都在容器回环网络，禁用公共引导和中继。需要一张实际 CUDA 显卡；`--init` 回收测试子进程。输出路径在容器临时目录，若需保留证据，可绑定一个空的本地输出目录并指定 `--output-dir`。

验证包括：

- 随机 Llama 小块分别以 FP16、INT8、NF4 完成 8-token 预填充和 3 次 KV 续算，检查输出/缓存有限值。
- 创建两层随机 Llama，由两个独立工作进程固定承载 `0:1`、`1:2`，等待所有块为 **ONLINE**，仅路由到这两个 Peer ID。
- 完整分布式 logits 与本机参考模型比较，之后在同一个会话连续生成 3 token。
- 可选 `--fault-test` 在会话中杀死一个工作进程，下一步必须有界失败；没有缺失块时的伪造回复或无限重试。

这是同一物理 GPU 上的协议/算子测试。随机权重没有对话能力；错误容差不是质量评估；常量测试路由分数不是吞吐实测。本测试不验证 13B 容量、真实多机网络速度或故障后的块重建，这些仍属于后续平台接入与硬件验收。

## 离线模型清单

`python -m petals_backend.prepare` 为已经下载的标准 Llama safetensors 目录生成不可变清单，不联网下载，不接受 Pickle 权重或远程 Python 模型代码。例如在包含实际模型文件的可写 `/models/team-model` 挂载中执行：

```text
python -m petals_backend.prepare --model-dir /models/team-model --id team-model-nf4 --name "Team model" --revision <固定模型版本> --tokenizer-revision <固定tokenizer版本> --quantization nf4
```

要求 `config.json`、`tokenizer_config.json`、tokenizer 数据和 safetensors 权重。聊天模板使用目录内 `chat_template.jinja` 或 tokenizer 配置中的明确字符串；缺失时必须通过 `--chat-template` 提供，不能猜测聊天格式。将输出 JSON 粘贴到控制台“模型目录 → 登记 Petals 封装模型”，或 POST 到 `/api/v1/model-artifacts`。

清单固定模型、tokenizer、聊天模板、配置和全部权重分片的字节数及 SHA-256。运行时再次校验清单摘要和每个文件；拒绝链接、路径越界、索引指向未验证分片和未纳入清单的可替代配置。目录应以只读方式挂到运行容器。重复准备相同清单可以读取原结果，更换版本、精度或模板必须准备新目录。

输出的 `block_mib` 是 Petals 参数量估计上浮 15% 后的每层 GPU 权重预算；`load_ram_mib` 覆盖两倍最大分片、FP32 嵌入/输出权重和 1 GiB 余量。这些是规划估计，仍需额外 KV/运行预算和实际加载预热，不能当作观测到的显存/内存占用。

`kv_bytes_per_token_per_layer` 固定 FP16 KV 每层每 token 字节数。调度同时检查每卡权重和 KV 上限；运行时检查封装清单与调度预算一致，再核对真实架构所需 KV。所有候选节点目前都需要完整、相同的已封装目录，即使只加载其中一段模型块。推荐使用原始分片 safetensors；单个超大权重文件会产生较高加载 RAM 预算。

## 启动私有集群

先运行上面的固定版本构建脚本，再在项目根目录构建 Agent：

```powershell
docker build -f deploy/compose/Dockerfile.petals-agent -t compute-platform-petals-agent:local .
```

将 `.env.petals.example` 复制为不提交的 `.env.petals`，填写实际主机私网 IP、唯一物理节点 ID、模型目录、控制面地址和共享令牌。基础镜像与 Agent 镜像需在各主机准备一致版本，正式分发应使用镜像 digest。

1. 在管理机器启动引导：`docker compose --env-file deploy/compose/.env.petals -f deploy/compose/docker-compose.petals.yml --profile bootstrap up -d bootstrap`。
2. `docker compose --env-file deploy/compose/.env.petals -f deploy/compose/docker-compose.petals.yml logs bootstrap` 输出当前私有 `/ip4/.../tcp/31330/p2p/...` 地址，把它填写为每台机器的 `PETALS_INITIAL_PEERS`。引导和工作节点密钥保存在各自卷内，不复制工作节点身份给另一台机器。
3. 在每台 GPU 机器启动：`docker compose --env-file deploy/compose/.env.petals -f deploy/compose/docker-compose.petals.yml --profile agent up -d edge-agent`。同一物理 GPU 只登记一次。保持 `init: true`；Agent 是容器的主要工作进程。
4. 私网放通管理端控制面端口、节点 `9090` 和 `31332`、引导 `31330` 与只读监控 `31331`。默认不使用公共 DHT、外部中继、自动下载或公共测速。地址校验不替代团队 VPN/防火墙。
5. 控制台启用实际节点，建立 **2–8 节点的已测量网络组**，完成双向测量。不同地点可显式放宽组策略，但仍需实际测量达到该策略；未测量、过期链路不能创建新的协作部署。
6. 选择封装模型和 Petals，预览连续块、每卡估计权重与主机 RAM，再创建部署。每台先逐文件验证、测量本机计算速度和加载指定块；全部就绪后，协调节点启动网关并实际生成 1 token 才开放请求。可以将部署加入“聊天入口”。

输入仅支持 `system/user/assistant` 文本消息，`n=1`、`max_tokens`（或 `max_completion_tokens`）、`temperature`、`top_p`、`stream` 和 `stream_options.include_usage`。工具、图片及其他参数会明确拒绝。每个网关只有一个会话；取消后直到生成线程和远端会话退出才释放本地额度，不能因 HTTP 已断开就并行启动下一请求。

当前块列表和 Peer ID 在一次分配中固定；没有把所有互联网显存自动汇集的发现调度，也不自动迁移已开始的会话。块内部运行池可以在原节点重建，受影响请求失败；节点进程失败则按平台失败/停止确认流程释放资源。选择替代节点需要新建部署，并调整入口成员；不重放旧请求或承诺恢复丢失 KV。

## 每个服务的观测

- Agent、控制连接、网络探测、协调网关沿用平台监控。新增 `petals-worker`：块范围和身份验证后的状态、活动/累计会话、错误、序列化收发字节、推理步数、KV 已用/容量、CUDA 分配/保留字节。
- 工作服务字节是推理 protobuf 载荷，不含 DHT、TCP/加密开销；其会话数也不是 token 数。`compute_tokens_per_second_per_block` 为启动时随机单块本机测试，仅作路由分数，不是端到端生成速度。实际 token 用量由生成器返回。
- 引导 `/health` 和 `/metrics` 位于私网 `31331`；状态取实际 DHT 子进程存活，流量取专用容器网络命名空间，缺失时省略而不填 0。在管理端 `services.json` 的 `targets` 追加：

```json
{"id":"petals-bootstrap","name":"Private Petals bootstrap","url":"http://你的私网IP:31331/health","traffic_url":"http://你的私网IP:31331/health"}
```

重启控制面加载监控配置。容器网络流量与模型载荷口径不同，不能跨层相加。原始进程日志可从节点工作进程日志入口查看；长期集中采集属于后续日志工作。

## 运行链路与租约测试

```powershell
docker run --rm --init --gpus all --network none --shm-size 1g --entrypoint python compute-platform-petals-compat:local -m petals_backend.runtime_smoke
docker run --rm --init --gpus all --network none --shm-size 1g --entrypoint python compute-platform-petals-agent:local -m petals_backend.agent_smoke
```

第一项运行实际引导、两个有监控的固定块工作服务与聊天网关，验证 JSON/SSE、真实 token 计数、取消释放和中途杀死一个块后有界失败，失败尾部不能出现 `[DONE]`。

第二项运行真正的 Go Agent、Python 网关和 GPU 工作进程，使用测试控制连接下发租约；另一个块仍是同 GPU 上的独立测试进程，**没有向真实控制面重复登记显卡**。断开控制连接后保持正常 45 秒租约，验证停止 GPU 子进程、拒绝新请求和恢复后的失败/停止确认。这是执行器协议验证，不能代替两台真实机器的完整调度与性能验收。
