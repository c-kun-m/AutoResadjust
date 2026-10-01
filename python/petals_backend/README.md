# 私有 Petals 兼容性验证

当前是可复现的兼容性与故障验证入口。控制面的 `petals` 后端尚未开放；不能把此镜像作为已接入租约、资源预留和服务监控的 Agent 使用。

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

要求 `config.json`、`tokenizer_config.json`、tokenizer 数据和 safetensors 权重。聊天模板使用目录内 `chat_template.jinja` 或 tokenizer 配置中的明确字符串；缺失时必须通过 `--chat-template` 提供，不能猜测聊天格式。输出的 `safetensors` 目录项用于后续平台接入，当前控制面仍只接受 GGUF。

清单固定模型、tokenizer、聊天模板、配置和全部权重分片的字节数及 SHA-256。运行时再次校验清单摘要和每个文件；拒绝链接、路径越界、索引指向未验证分片和未纳入清单的可替代配置。目录应以只读方式挂到运行容器。重复准备相同清单可以读取原结果，更换版本、精度或模板必须准备新目录。

输出的 `block_mib` 是 Petals 参数量估计上浮 15% 后的每层 GPU 权重预算；`load_ram_mib` 覆盖两倍最大分片、FP32 嵌入/输出权重和 1 GiB 余量。这些是规划估计，仍需额外 KV/运行预算和实际加载预热，不能当作观测到的显存/内存占用。
