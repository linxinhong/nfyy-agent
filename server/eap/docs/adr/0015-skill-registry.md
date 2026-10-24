# ADR-0015 治理 Skill Registry 与服务器签名供应链（TASK-016）

状态：已接受　日期：2026-09-01　关联任务：TASK-016

## 决策

### 1. 确定性打包与不可变 name@version

`BuildArchive` 以排序文件 + 固定 mtime 生成 tar.gz（同输入恒同字节同摘要）；name@version 唯一约束使已发布版本不可覆盖（冲突 409）。发布体积/文件数/单文件大小上限防无界资源占用。

### 2. 服务器权威校验（不信任 CLI 结果）

路径安全（相对路径、拒绝 ../、反斜杠、绝对路径；归档仅普通文件——symlink 天然不可能）、内容扫描（shell 脚本/eval/curl|sh/密钥形态，服务端重复执行）在打包与提取两侧都执行；下载侧 `ExtractBounded` 再校验（有界读取、类型、路径、重扫描）。

### 3. 生命周期与角色

pending_review → approved → released → deprecated/revoked；发布需工作空间成员（agk key），review/release/revoke 需控制面会话 + maintainer 角色；未批准不可发布（直接稳定发布在生命周期中不可表达）；revoked 下载 404；跨 Workspace 资源不可区分 404。

### 4. 包签名密钥：独立命名空间 + 轮换保留历史

签名密钥 kid 强制 `pkg-` 前缀（内部 JWT 密钥被显式拒绝）；私钥仅存在于网关装配（PackageKeys 注入），公钥经 `/.well-known/package-keys.json` 发现（活跃 + 历史，轮换后历史签名仍可离线验证）；新发布签名自动使用活跃密钥。签名对象为 archive 摘要（hex），下载响应头携带 digest + JSON envelope（kid/alg/digest/signature）。

### 5. 审计

published/reviewed/released/revoked 全部经 RequireWrite 可靠路径、trace 关联、无原始内容——投递失败即转换失败（fail closed）。

## 验证

`make verify-skill-registry` = verify-device-bootstrap（含全链前级）+ `TestSkillRegistryIntegration`：完整正/负矩阵（重复 409、未发布不可下载、agk 不能评审、跨 Workspace 404、路径逃逸与扫描发现拒绝、篡改摘要不通过离线验证、轮换后历史可验证且新发布用新 key、吊销后 404、四类审计齐备）——真实 PG/Redis。
