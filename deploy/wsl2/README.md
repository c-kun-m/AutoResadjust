# WSL2 节点准备

每台 Windows GPU 服务器都执行一次：安装最新 NVIDIA Windows 驱动，启用 WSL2，安装 Ubuntu，并在 Docker Desktop 中打开 WSL2 backend。Linux 子系统里不要再安装独立的 Linux NVIDIA 驱动；驱动由 Windows 侧向 WSL2 暴露。

执行 `check-node.ps1` 验证 `nvidia-smi` 和 Docker GPU 运行时。随后在每台服务器启动一个 `edge-agent`，使用固定的 `--node-id`、`--site` 和控制面地址。节点之间只上报自己的资源和任务状态，不做跨服务器模型切分。
