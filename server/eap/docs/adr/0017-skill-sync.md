# ADR-0017 Skill 托管安装与 Workspace 同步（TASK-018）

状态：已接受　日期：2026-09-01　关联任务：TASK-018

## 决策

### 1. lockfile 定义的托管面

`.eap/skills.lock.json` 记录每个安装项（name/version/digest/client/install_dir/文件摘要表）。**只有 lockfile 清单内的文件是托管的**：升级/移除只收敛托管文件，用户在同一目录的非托管文件（笔记、本地修改）永不触碰；安装前碰撞检查——目标位置存在非托管同名文件即拒绝（409 语义，不静默覆盖）。

### 2. 原子应用与中断恢复

新版本文件先全部写入临时 staging 目录，再移入安装根；任何一步失败（mkdir/write/lockfile 写入）恢复先前工作状态（prior 状态在删除前捕获）。lockfile 写入成功才算同步完成——中断的 apply 不会留下半激活状态。

### 3. 下载侧三重校验

字节摘要重算比对注册表 digest → 签名 envelope 离线验证（发现文档密钥，轮换兼容）→ 提取时有界重扫（路径/类型/内容）。下载失败（worker 不可用）同样不产生部分状态。

### 4. CLI 接线

`eap skill sync <name> <version> --client harness` 与 `eap skill remove`：下载走公开注册表端点（agk/设备凭据），签名密钥经发现文档；凭据不落配置。

## 验证

`make verify-eap-skill-sync` = verify-eap-cli（含全链前级）+ skillsync 单元/竞态套件：安装内容断言、幂等重放、升级内容替换、篡改摘要/签名与恶意归档 fail closed、worker 失败无部分状态、策略移除仅收敛托管文件。
