# ADR-0018 Agent 发布分发与同域票据安装（TASK-019）

状态：已接受　日期：2026-09-01　关联任务：TASK-019

## 决策

### 1. 平台匹配的发布目录

`agent_releases`（client/os/arch/version 唯一，workspace 作用域，released/revoked 生命周期，minimum_version 策略）。列表与签票 SQL 内同时过滤 workspace、平台与 status=released——跨 Workspace/错误平台/未知版本统一 404（不可区分），不存在未知 OS 回退安装。

### 2. 同域短时下载票据

ticket 为 32 字节随机值（无存储路径/凭据语义），2 分钟过期，**签发与兑换两侧都重新校验**授权、Workspace 与发布状态；兑换为原子 UPDATE（`redeemed_at` 单次置位，重放即 403）。载荷不经对象存储直链——同域端点流式返回，响应头携带 digest + 签名 envelope（TASK-016 的 pkg- 密钥体系复用）。

### 3. CLI 侧验证与原子激活

`agentdist.Install`：下载字节重算摘要比对 → 签名离线验证 → staging 写入 → rename 原子激活 → 激活后回读校验，任何失败**保留先前安装**（rollback 语义，forbidden"验证前覆盖可用安装"闭合）。

### 4. 边界

无通用包管理、无特权远程安装、无灰度策略（Workspace policy 表面由 TASK-011 承载）；真实 Harness 二进制/签名公钥的资格验证属操作员运行簿（卡片 escalation 路径）。

## 验证

`make verify-agent-distribution` = verify-eap-skill-sync（含全链前级）+ `TestAgentDistributionIntegration`：列表/签票/兑换返回载荷与 digest 头、票据重放 403、跨 Workspace 列表无泄漏且签票 404、未知版本 404、download_tickets 记录在库——真实 PG/Redis。`packages/agentdist` 单测：原子安装、票据重放、吊销、篡改字节、prior 保留、版本策略。
