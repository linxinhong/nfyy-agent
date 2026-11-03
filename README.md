# Yuanpu Agent

Electron 图形界面与 Node.js SEA Runtime 解耦的 pnpm monorepo。工程组织参考 OpenWork，独立 Runtime 更新机制参考 Kimi Code。

## 架构

- `apps/app`：React/Vite renderer，只能访问 preload 暴露的窄接口。
- `apps/desktop`：Electron main/preload，负责窗口、Runtime 生命周期与桌面端更新。
- `apps/runtime`：Node SEA sidecar，承载 Agent 与本地服务，可以独立于桌面壳更新。
- `server/backend`：独立 Go 中心服务及技能目录（8788）；`server/web`：中心网站（8789）。见 [开发与部署说明](server/README.md)。
- Pi：固定版本的官方 npm 包；依赖边界、内置 MCP 与升级验证见 [Pi 集成说明](docs/pi-integration.md)。
- `packages/yuanpu-protocol`：Renderer、Electron 和 Runtime 共用的协议版本与类型。
- `packages/yuanpu-runtime`：Yuanpu 后端实现，包含配置、Pi 适配、能力注册表、MCP 与能力包管理。
- `packages/yuanpu-server-client`：中心服务的 TS SDK，接口类型由 OpenAPI 生成。

Electron 首次使用安装包中携带的 Runtime。独立更新会下载到 Electron `userData/runtime/.staging`，校验文件大小和 SHA-256，执行 `--version` 冒烟测试，并在下次启动时原子切换。Runtime API 只监听 `127.0.0.1` 的随机端口。

## 本地开发

Python 能力包与统一能力入口的设计见 [设计文档](docs/python-capabilities.md)，实施顺序与验收见 [任务入口](.tasks/README.md)。当前已打通开发态 Python MCP 示例与 Node SEA 调用链；面向最终用户的跨平台冻结制品、安装和更新仍由后续任务交付。

要求 Node.js 24.15+ 和 pnpm 11。开发 Go 中心服务或运行完整 `pnpm check` 还需要 Go 1.26+；Compose 验证需要 Docker。

```bash
pnpm install --frozen-lockfile --ignore-pnpmfile
pnpm check
pnpm dev
```

`pnpm dev` 会构建 Runtime 和 Electron main/preload，同时启动技能市场服务、Vite 和 Electron。

`pnpm dev:server` 启动 EAP 知识服务中心；`pnpm check:server` 验证 Go 网关、整理运行时与 Portal。EAP 需要 PostgreSQL 和 Redis；旧中心的部署冒烟仍可通过 `pnpm smoke:server:compose` 执行。详见[中心说明](server/README.md)。

## 本地 Agent 配置

Runtime 使用 Node 的系统主目录解析创建跨平台配置根目录：macOS/Linux 为 `~/.yuanpu`，Windows 为 `%USERPROFILE%\\.yuanpu`。首次启动会创建以下边界：

- `app/config.json`：Yuanpu 全局配置，包括当前模型的 `provider`、`model` 和可选的 `catalogUrl`。
- `app/auth.json`：模型供应商的 API Key 与 OAuth 凭据，由设置界面写入。
- `app/models.json`：按需创建，仅保存自定义模型和端点定义；内置模型由 Pi 模型目录提供。
- `agent/`：Pi 会话、模型缓存、插件设置、记忆和本地技能。
- `packages/`：从技能市场安装的能力包及其独立配置。
- `workflows/`：Yuanpu 工作流数据。

默认模型也可从 `OPENAI_API_KEY` 环境变量读取密钥。请在设置界面的“模型”页面选择内置模型、填写 API Key，或添加自定义端点。`app/config.json` 只保存当前的 `provider` 和 `model`；密钥保存在 `app/auth.json`，自定义端点保存在 `app/models.json`。旧版 `agent/auth.json`、`agent/models.json` 和 `config.json` 中的模型端点字段会在启动时迁移。

本地技能放在 `~/.yuanpu/agent/skills/<name>/SKILL.md`。技能市场安装的是可包含技能、专家角色、工作流、扩展或服务连接的“能力包”，普通用户只需要理解“技能”这一入口。

## 构建产物

构建当前平台的 SEA Runtime：

```bash
pnpm build:native
pnpm smoke:native
pnpm package:native
```

构建当前平台的 Electron 安装包（会先构建并嵌入 SEA Runtime）：

```bash
pnpm package:desktop
```

Runtime 产物位于 `apps/runtime/dist-native/artifacts/`，桌面产物位于 `apps/desktop/release/`。

## CI/CD

- `CI`：对 push 和 PR 执行类型检查、构建和测试。
- `Runtime Bundle`：既可手动构建三个平台的独立 SEA Runtime，也供正式发布复用。
- `Desktop Bundle`：既可手动构建三个平台的 Electron 安装包，也供正式发布复用。
- `Release`：推送 `v*.*.*` tag 后，同时发布桌面安装包、Runtime 裸二进制和 `manifest.json`。

Runtime manifest 包含协议版本、最低桌面版本、各平台 URL、大小和 SHA-256，供桌面端的独立更新器消费。

```bash
git tag v0.1.0
git push origin v0.1.0
```

激活 GitHub Actions：在仓库的 **Settings → Actions → General** 将 Actions 设为允许运行；提交 PR 或推送到 `main` 会自动运行 `CI`，推送符合 `v*.*.*` 的 tag 会运行 `Release`。也可以在 **Actions** 页面手动运行 `Runtime Bundle` 或 `Desktop Bundle`。

正式发布前，在 **Settings → Secrets and variables → Actions** 配置以下仓库级 secrets：

- `YUANPU_ARTIFACT_SIGNING_KEY_BASE64`
- `YUANPU_ARTIFACT_TRUST_ROOT_PUBLIC_KEY_BASE64`
- `YUANPU_ARTIFACT_SIGNING_KEY_ID`
- `YUANPU_ARTIFACT_BASE_URL`

macOS 签名和公证是可选的。未配置证书时，GitHub Actions 产出 ad-hoc 签名的 DMG/ZIP，用户首次打开会看到 Gatekeeper 提示。面向公众分发时，在同一位置再配置以下 secrets，`Desktop Bundle` 会自动使用 **Developer ID Application** 证书签名、经 Apple 公证并校验：

- `CSC_LINK`：Developer ID Application 证书导出的 `.p12`，base64 编码（`base64 -i cert.p12`）
- `CSC_KEY_PASSWORD`：导出 `.p12` 时设置的密码
- `APPLE_API_KEY_BASE64`：App Store Connect API Key（`AuthKey_XXXX.p8`），base64 编码
- `APPLE_API_KEY_ID`、`APPLE_API_ISSUER`：该 API Key 的 Key ID 和 Issuer ID

证书和密钥只保存在 GitHub Secrets 与密码管理器中，不要提交到仓库。

桌面安装包只包含 `apps/desktop`、Renderer、Runtime 和内置 Python 能力包；`server/` 是独立服务，不参与桌面打包。
