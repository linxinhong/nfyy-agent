# ADR-0019 Portal Skill/Agent 治理操作面（TASK-020）

状态：已接受　日期：2026-09-01　关联任务：TASK-020

## 决策

### 1. 服务端目录替代静态 fixture

Marketplace 页（/portal/marketplace）消费 `GET /enterprise/v1/skills?q=` 公共契约（Workspace 作用域 + 中央 policy 过滤 + 搜索参数），与 CLI 同源——Portal 不维护第二目录，静态 fixture 移除（forbidden 闭合）。响应只含 name/version/status/digest，无 payload/存储数据。

### 2. 授权边界在服务端

评审/发布/吊销按钮对所有访问者渲染，但**授权在服务端**（agk key 无控制面会话 → 401；非 maintainer → 403）——"仅隐藏按钮"的 forbidden 不可表达。浏览器会话（HttpOnly cookie/内部 JWT）不接触设备刷新凭据、pkg 私钥或存储引用。

### 3. 变更展示服务器结果 + trace id

每个变更的错误以冻结 Problem schema 呈现（code + message + trace_id），成功以服务器返回状态刷新目录。

### 4. 统一网关部署

SPA fallback（/portal/marketplace 刷新 200）与 API 优先级（TASK-014 契约）在集成测试中同网关断言。

## 验证

`make verify-portal-registry` = verify-agent-distribution（含全链前级）+ `TestPortalRegistryIntegration`：目录/搜索/跨 Workspace 隔离/非维护者拒绝/SPA 回退与 API 优先级——真实 PG/Redis。
