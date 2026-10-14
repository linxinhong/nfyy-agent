# YuanpuAgent 技能与能力包系统

YuanpuAgent 对普通用户统一呈现“技能”。技能背后是一个可版本化的能力包，可包含 Pi skills、extensions、prompts、themes、专家角色、工作流或服务连接。Pi 上游包保持不变，YuanpuAgent 只通过公开的 package/resource loader 契约加载能力包。

## 用户目录

用户数据按所有权分为四个根目录：

```text
~/.yuanpu/
├── app/             # YuanpuAgent 全局配置、更新、缓存和日志
├── agent/           # Pi Agent 目录：settings、skills、memory、sessions 等
├── packages/        # 市场能力包、配置和安装事务
└── workflows/       # 用户工作流与项目运行状态
```

能力包安装成功后，安装器把已启用包的绝对路径同步到 `~/.yuanpu/agent/settings.json` 的 `packages` 字段。Pi 的 `DefaultResourceLoader` 因此可以按标准 Pi package 规则发现 extensions、skills、prompts 和 themes。

## 安装流程

1. “技能市场”优先查询配置的 Yuanpu 目录服务；未配置目录时回退到 npm registry 的 `pi-package` 搜索。
2. 搜索结果始终使用精确版本安装，例如 `npm:example-plugin@1.2.3`。
3. 也可以输入固定到完整 40 位 commit SHA 的 GitHub HTTPS 地址。
4. 安装器在 `.staging/<transaction>` 中创建独立 package root，并用 Arborist 解析完整依赖树。
5. npm lifecycle scripts 始终关闭；允许包管理器为当前平台解析出的预编译 N-API 依赖，安装后必须通过真实扩展加载检查。
6. 验证 Pi manifest 或约定资源目录后，整个事务目录被原子移动到 `installed/`。
7. 状态与 Pi settings 写入后，runtime 释放当前 Pi 会话；下一次对话使用新的插件集合。
8. runtime 使用 Pi resource loader 验证扩展入口；加载失败的插件会保留、自动停用并记录错误。

安装失败不会替换已安装版本，并会清理暂存事务。更新同名插件成功后才删除旧容器。

## 安全边界

- Pi extension 拥有与 runtime 相同的本机代码执行权限，不是安全沙箱。
- 界面在安装前显示来源并要求用户确认。
- 不执行安装脚本；允许包管理器为当前平台解析出的预编译 N-API addon，并在安装后通过真实扩展加载检查。
- npm 来源必须使用精确 semver；Git 来源必须固定 commit，避免同一来源静默漂移。
- 插件各自拥有独立 `node_modules`，避免第三方依赖相互覆盖。
- 用户的本地 `agent/skills`、`agent/memory` 和其他 Agent 数据不属于能力包卸载范围。

## 本机协议

Electron renderer 只通过 preload 暴露的 typed bridge 调用插件功能。Electron main 将请求转发给带随机 bearer token 的 loopback runtime：

- `GET /v1/plugins/search?q=...`
- `GET /v1/skills/local`
- `GET /v1/plugins`
- `POST /v1/plugins/install`
- `POST /v1/plugins/state`
- `POST /v1/plugins/uninstall`
- `GET /v1/plugins/config?name=...&scope=user|workspace`
- `POST /v1/plugins/config/validate`
- `POST /v1/plugins/config/save`
- `POST /v1/plugins/config/reset`

runtime 是唯一执行安装事务和修改插件状态的进程。

## 能力包配置

- MCP 使用 Pi 1.0 内置实现，在技能页点击“MCP 服务”直接配置，无需安装 `pi-mcp-adapter`。用户级配置为 `~/.yuanpu/agent/mcp.json`，工作区覆盖为 `<workspace>/.pi/mcp.json`。
- Yuanpu-aware 能力包可在 `package.json` 的 `yuanpu.config.schema` 声明包内 JSON Schema。用户配置写入 `~/.yuanpu/packages/config/<package>/`，不会进入可替换的安装目录。
- 配置先校验再原子写入；保存或重置后 runtime 结束当前 Pi 会话，下一次对话加载新配置。
- 名为 API Key、Token、Secret、Password 或 Authorization 的敏感字段只接受 `${ENV_NAME}` 或 `$env:ENV_NAME` 引用，拒绝明文落盘。

能力包作者可用以下清单声明配置能力：

```json
{
  "yuanpu": {
    "config": {
      "schema": "./config.schema.json",
      "schemaVersion": 1,
      "title": "示例技能",
      "description": "配置示例技能的服务地址。",
      "required": true,
      "scope": ["user", "workspace"],
      "reload": "session"
    }
  }
}
```

`schema` 必须指向插件安装目录内的 JSON Schema；越界路径会被拒绝。未声明此清单且没有内置适配器的插件仍可安装和运行，但界面不显示“配置”。工作区配置由 YuanpuAgent 按工作区绝对路径散列后独立保存，避免写入插件目录或相互覆盖。

## 当前限制

- 需要本机编译或依赖安装脚本生成 addon 的插件仍不兼容；只有包内已提供当前平台预编译产物的插件可用。
- Pi 内置 MCP 通过 `tool_search` 发现后调用 `mcp__` 工具；签名 Python 制品等 Yuanpu 受管能力仍通过 `search_capabilities` 和 `execute_capability` 两个元工具暴露。迁移与版本约束见 [Pi 集成说明](pi-integration.md)。
- 插件启停通过重建 Pi 会话生效，当前正在执行的 prompt 不会被中途热替换。
- 系统密钥库尚未接入；首版使用环境变量引用，避免把密钥写入插件配置文件。
