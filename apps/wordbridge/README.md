# WordBridge（南医小助 Word 改稿桥）

让 Agent 通过 MCP 驱动 **Microsoft Word for Mac**：读取选区或全文、查找文本，并以**修订（Track Changes）**方式写回，由用户在 Word 中逐条接受或拒绝。随 macOS 安装包内置，对应内置技能 `apps/app/agents/skills/word-revision`。

## 适用范围

- 仅 macOS Apple silicon（`darwin-arm64`）。锁定依赖在 Intel Mac 上没有二进制 wheel，Intel 构建会跳过 WordBridge。
- 需要本机安装 Microsoft Word（`/Applications/Microsoft Word.app`），否则桌面端不会启动桥。
- Windows COM 驱动（`com_worker.py`）保留在源码中，目前不打包。

## 结构

| 路径 | 作用 |
| --- | --- |
| `wordbridge/` | Python 桥：MCP（Streamable HTTP，`127.0.0.1:9877/mcp`）、加载项 HTTPS 服务（`localhost:9879`）、修订引擎 |
| `addin/` | Office 加载项：manifest、任务窗格、图标 |
| `run_bridge.py` | 入口 |
| `requirements.txt` | 直接依赖 |
| `requirements-closure.txt` | 完整依赖闭包（版本锁定） |
| `requirements-macos-arm64.txt` | 带 SHA-256 的 macOS arm64 安装清单，打包时 `--require-hashes --no-deps` 安装 |
| `scripts/lock-macos-arm64.sh` | 由闭包重新生成哈希清单 |
| `scripts/smoke-bundle.mjs` | 打包后冒烟：启动、鉴权、端口、退出 |

## 打包

`pnpm package:desktop` 在 macOS 上会运行 `apps/desktop/scripts/prepare-wordbridge.mjs`：

1. 下载固定版本的 python-build-standalone（CPython 3.12.7），校验 SHA-256。
2. 按哈希清单安装依赖，执行 `pip check`，删除 Tk、测试、头文件等无用部分。
3. 预编译字节码（`unchecked-hash`），并做一次导入冒烟。
4. 产物在 `apps/wordbridge/dist/`，由 electron-builder 放进 `南医小助.app/Contents/Resources/wordbridge/`，随应用一起签名和公证。

## 运行时（`apps/desktop/src/wordbridge-host.ts`）

桌面端在窗口出现后启动桥，退出 App 时停止它；不安装 LaunchAgent，不常驻。

- 状态目录：`~/Library/Application Support/Yuanpu Agent/wordbridge/`（配置、证书、region 数据库、日志）。
- 证书：每台机器本地生成 `localhost` 叶子证书（`CA:FALSE`，仅 serverAuth），私钥不随安装包分发。首次会请求用户确认把它加入登录钥匙串的 SSL 信任；拒绝后同一证书不再询问。
- 加载项：把 manifest 写到 Word 的 `wef` 目录（`mindlink-wordbridge.xml`）。在 Word 中选择 **开始 → 南医小助 → 打开南医小助**。
- 鉴权：每次启动生成随机令牌，只交给 Runtime；MCP 和调试 REST 路由（`/health` 除外）都要求 `Authorization: Bearer`。Runtime 以内置 MCP 服务 `wordbridge` 注册，令牌只以环境变量引用出现，不写入任何配置文件。
- 旧版调试 WebSocket（9876）关闭；加载项 WebSocket 只接受任务窗格自身的 Origin。
- 设置 `YUANPU_WORDBRIDGE_DISABLED=1` 可关闭；开发态可用 `YUANPU_WORDBRIDGE_ROOT` 指向已准备好的 `dist/`。

## 可选功能

- 脱敏：默认关闭。设置 `DESENSITIZE_URL`（和 `GATEWAY_API_KEY`）后，读取的文本会先发往该端点做 token 化。
- 长文翻译：需要在 App 环境中提供 DeepSeek / Kimi / SiliconFlow 的 API Key。
