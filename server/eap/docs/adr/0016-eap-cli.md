# ADR-0016 eap CLI 基础与企业 doctor（TASK-017）

状态：已接受　日期：2026-09-01　关联任务：TASK-017

## 决策

### 1. 单二进制命令壳（Cobra + 配置优先级链）

`cmd/eap` 单二进制；Cobra 命令壳（login/logout/whoami/auth status/workspace list+use+status/model list/mcp list+status/config show+path/doctor），`--json` 全覆盖。配置优先级：flags > 环境（EAP_*） > 项目 `.eap/workspace.yaml` > 用户 `~/.config/eap/config.yaml`。**凭据只来自环境变量或 OS Keychain**——项目/用户配置文件永远不含凭据（forbidden 闭合）。

### 2. 凭据：Keychain 接缝 + 无明文回退

`KeyringStore` 接缝隔离平台实现（macOS `security`、Linux `secret-tool`）；无明文文件回退（out of scope 明确）。设备流登录把 refresh 凭据写入 Keychain；`EAP_API_KEY` 提供无头模式。所有输出（人读/JSON/错误/诊断）经 redact() 过滤 agk key/JWT/Bearer/令牌字段。

### 3. 稳定输出与退出码

退出码 0/1/2（ok/runtime/usage）稳定；网络与策略失败是错误，绝不输出空成功结果。JSON 输出经契约测试。

### 4. doctor 诊断

连通性（healthz）、凭据（env/keychain 状态，不泄露值）、Workspace 选择、模型可用性（经认证的 /v1/models）、MCP 授权（token exchange）、最低版本。失败项即发现，exit 非零。

### 5. 跨平台构建

`make cli-cross-build`：windows-amd64 / darwin-arm64 / linux-amd64 单二进制产物。

## 验证

`make verify-eap-cli` = verify-skill-registry（含全链前级）+ cli-cross-build + `packages/cli` 测试：配置优先级（flag>env>project>user）、redaction（错误输出不含 agk 密钥）、设备登录→whoami→workspace list/use/status→model list→mcp list→doctor→logout 全旅程（对网关契约桩）、吊销后闭合。
