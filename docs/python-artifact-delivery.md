# Python 能力制品与安全更新

本文记录 `python-mcp` 能力包的已实现交付边界。通用进程模型、授权和 MCP 两工具契约见 [python-capabilities.md](./python-capabilities.md)。

## 制品形态

`apps/python-capabilities` 使用固定版本 PyInstaller `onedir`，把 Python 解释器、FastMCP 服务和运行依赖构建成目标平台目录，再生成 `tar.gz`。目标机不需要 Python、pip、uv，也不会在安装或首次调用时从 PyPI 补依赖。

构建入口：

```sh
pnpm build:python-artifact
pnpm smoke:python-artifact
```

烟测将 `PATH` 指向一个空目录后直接执行冻结程序。`pnpm smoke:native` 进一步让 Node SEA 通过 stdio 调用冻结后的真实 MCP 服务，验证 `search_capabilities` 和 `execute_capability` 全链路。

桌面打包把 `dist-artifact/bundle` 放入 `resources/capabilities/builtin.python.echo`。生成目录只属于本地/CI 构建输出，受 `.gitignore` 管理，不提交仓库。

## 签名与信任根

manifest 使用 Ed25519 对去掉 `signature` 字段后的确定性 JSON 签名。客户端 `CapabilityArtifactManager` 的信任根只能由宿主本地构造参数提供；远端 catalog、manifest 和制品都不能新增或替换信任根。

正式构建必须注入：

- `YUANPU_ARTIFACT_SIGNING_KEY_FILE`：Ed25519 私钥 PEM 文件；
- `YUANPU_ARTIFACT_SIGNING_KEY_ID`：与客户端预置公钥匹配的稳定 ID；
- `YUANPU_ARTIFACT_TRUST_ROOT_PUBLIC_KEY_BASE64`：供 Desktop/Runtime 构建矩阵预置的公钥 PEM（Base64）；
- `YUANPU_ARTIFACT_BASE_URL`：不可变制品发布基址。

GitHub Release 场景的基址应指向 tag 目录之前，例如
`https://github.com/linxinhong/yuanpu-agent/releases/download/`；汇总脚本会追加具体 tag 和文件名。

没有发布凭据时，构建脚本只生成一次性的开发密钥和 `development: true` 的本地 trust-root 文件，用于测试及本次桌面包内部装配。它不具备生产发布资格。当前生产密钥托管、轮换和正式发布仍为 **UNVERIFIED**，不得把开发根上传为生产根。

Tag Release 采用 fail-closed 门禁：私钥、公钥、key ID、下载基址任一缺失或公私钥不匹配即停止发布，不允许把临时开发根带入正式 Release。六个 Runtime/Desktop 构建 runner 只接收公钥，不接收长期私钥；私钥仅进入最终 publish job。Runtime 矩阵上传各平台能力归档，发布任务再汇总目标、重新计算大小与 SHA-256，并生成一个生产密钥签名的 `YuanpuEchoMcp-manifest.json`。本地或手动 CI 仍可使用明确标记的临时根做构建验证。

## 安装和恢复

安装事务使用由操作系统内核持有的本机 loopback 端口租约串行化。同一个 packages 根目录
稳定映射到同一端口；进程退出或崩溃时内核自动释放租约，不通过删除 stale 文件抢锁，因而
不存在旧锁检查与新 owner 替换之间的竞态。等待方只在 `EADDRINUSE` 时重试，取消信号仍可
中止等待。

进程外制品与旧 Pi 插件分开管理：

```text
~/.yuanpu/packages/
├── state.json                 # 原 npm/Git Pi 插件，保持兼容
├── installed/                # 原 Pi 插件
├── artifact-state.json       # python-mcp 活动版本
├── artifacts/<id>/<version>/<platform-arch>/
├── .staging/artifacts/
├── .artifact-install.lock
└── config/                   # 用户配置；更新/回滚不删除
```

安装顺序为：本地信任根验签 → Runtime 版本与平台匹配 → 有界下载及 SHA-256 → 拒绝穿越、链接和超限归档 → 暂存目录健康检查 → 不可变版本目录 → 原子改写活动状态。

跨进程排他锁防止并发安装。新旧版本使用不同目录，不覆盖正在运行的 Windows 文件；健康检查或状态切换失败时活动版本仍指向旧目录。显式 `rollback()` 只切换已有且入口仍存在的历史版本。离线查询活动版本只读取本地状态。

Python 制品不会写入 Pi `settings.packages`。`detectMcpOwnershipConflicts()` 检查 Yuanpu 与 Pi 内置 MCP 对同名连接的重复托管，安装端会拒绝冲突；请先在“MCP 服务”中停用相应连接。旧 `pi-mcp-adapter` 已由 [Pi 1.0 迁移](pi-integration.md) 退役，连接配置仍保留。

## Catalog 发布入口

目录服务提供：

- `GET /v1/capability-packages/builtin.python.echo/manifest`
- `GET /v1/capability-packages/builtin.python.echo/artifacts/<filename>`

服务端通过 `YUANPU_ARTIFACT_ROOT` 指向已经签名的输出目录；服务只原样返回 manifest 和不可变制品，不代替客户端验签。生产发布时构建使用的签名 URL 必须与实际制品地址一致。
开发 manifest 使用相对的 `artifacts/<filename>`；安装器以实际 manifest URL 为基址解析，但验签仍针对原始相对 URL，不会通过改写字段破坏签名。
`pnpm dev` 将本地 `apps/python-capabilities/dist-artifact` 交给 Go 中心，并将其 `bundle/trust-root.json` 公钥交给桌面 Runtime；首次缺少 manifest 或信任根时先运行 `build:python-artifact`。可通过 `YUANPU_ARTIFACT_ROOT` 与 `YUANPU_CAPABILITY_TRUST_ROOT_FILE` 显式覆盖；自定义路径缺失会停止启动，不自动重建或替换。开发启动仍使用正常的 manifest 验签和安装校验。
Artifact endpoint 只允许返回 manifest 中列出的文件名，发布目录中即使误放私钥或环境文件也不会被下载；畸形 URL 编码返回 400，不会抛出未处理异常。

## CI 平台

现有 GitHub Actions 矩阵保持为 `linux-x64`、`darwin-arm64`、`win32-x64`。Runtime 与 Desktop 工作流都安装固定 uv、在对应 runner 上构建和烟测 Python 制品；Desktop 随包携带当前平台目录。非当前 macOS 主机的无 Python 运行、系统签名提示及真实更新/回滚证据由最终跨平台验收任务记录，不能由本机结果推断。
