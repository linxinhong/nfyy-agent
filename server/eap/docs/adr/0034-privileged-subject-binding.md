# ADR-0034：平台管理员主体绑定与邮箱关联迁移边界

- 状态：Accepted（仅设计；运行尚未实施，生产 no-go 不变）
- 日期：2026-09-25
- 任务：冻结平台管理员主体绑定与邮箱关联迁移边界（TASK-069）
- 运行实现唯一 owner：TASK-065；仅设计，不授予生产权限
- 上游：ADR-0020、ADR-0030、ADR-0033；TASK-023/TASK-025

## 1. 冲突、证据与有意优先级

ADR-0020/TASK-023 已交付 subject-primary、email optional 以及 IdP-asserted email linking（包括目录协调/IdP 迁移）。ADR-0033 §2.1 则要求平台管理员绑定精确可信 issuer+subject，禁止以相同 email 合并。二者不能仅靠新的控制面 session helper 调和。

在本卡基线 `d7d7af0`，`packages/store/store.go:UpsertUser` 的 email 分支先 SELECT 同邮箱且不同 subject 的用户，再 UPDATE 该行 idp_subject，随后 subject UPSERT；还能同步 organization。`IDClaims`/users 没有 issuer namespace，旧函数不检查平台 grant 或历史 operation，也没有可靠身份关联审计。`apps/gateway/oidc.go:oidcCallback` 将标准 OIDC 或 Feishu 的 Subject/Email 送入该函数；标准验证检查 IdP 签名/iss/aud/nonce，`packages/oidc/oidc.go` 未携带 email_verified。`packages/oidc/feishu.go` 使用 server-side user_info 的 union_id，缺失时回退 open_id，返回值没有 subject kind。ADR-0020 明言 Feishu email 是目录断言而非用户验证；因此这里的历史“verified-email”指可信适配器的目录断言，不能虚称现实现验证了 email_verified。

另只读参考 TASK-065 分支 `9445662e0d8ea6d9114e24fbdef5af03f9567c99` 的 `migrations/000023_platform_admin_authority.sql`：grant FK 绑定 user_id/organization，保留 grant 历史和 session user_auth_epoch；它本身不冻结用户 issuer/subject。该未集成实施材料不等于本卡基线已有生产防护。

本 ADR 作为有意的后续限定：尚未进入 §3 永久保护范围的普通用户保留可信邮箱关联与无邮箱 JIT；受保护用户禁止普通路径重绑，即使 grant 已撤销；跨 issuer 不允许仅凭 email 自动合并。本 ADR 不撤销既有普通用户的关联结果，不改 TASK-025 Workspace roles additive 语义。跨 issuer 的历史无条件关联被明确收窄为受控迁移，原因是原 schema 不能分辨 IdP namespace，也不能证明同一自然人。涉及未知旧映射时封闭迁移，不猜测主体现属。

## 2. Canonical namespace 与不可覆盖历史

由同一 PostgreSQL 身份库维护可信 `identity_namespaces`：不可变 namespace_id、installation/organization、adapter kind、可信 issuer/tenant/application 范围和 subject_kind、配置 revision、启用状态。来源由受控部署 writer 配置，不接受 browser/header/email 域名建立映射；配置变化不得就地改变 namespace 含义。

- 标准 OIDC：namespace 固定到已验证 exact issuer 和受控 tenant/client 配置；subject 大小写敏感、不 trim/归一化猜测。issuer 或租户/信任配置发生身份语义变化须新 namespace；不得按 URL 相似合并。
- Feishu：从受控服务端配置及已验证 exchange/user_info 取得应用/租户边界；union_id namespace 至少含 tenant 与其可证明的应用作用域；无法证明跨 app 稳定范围则保守限制当前 app。open_id namespace 必须含 app/tenant、kind=open_id，与 union_id 永远不同。由 open_id 改为 union_id 是绑定迁移，不是 string 回填；受保护用户走 §5。不能从客户端选择 kind。
- 适配器须输出可信 `namespace_id + subject + email_assertion_provenance`；email 来源资格由部署审查的适配器策略决定。保留现有受控目录邮箱断言合同，不新增以浏览器声称 email_verified 绕过的路径。email 为可选属性，规范化只沿用现有 lower(email)，不做点号/加号等供应商猜测。

逻辑 schema seam：`user_identity_bindings(binding_id, user_id, namespace_id, subject, generation, bound_at, retired_at, evidence_id)`；每个 user 恰一 current binding，(namespace_id,subject) 永久唯一，包括 retired 行，防止旧主体被重新分配为另一个 user。迁移保留全部旧行；users.idp_subject 仅兼容投影，不能成为第二个可写身份源。`users.identity_protected_at` 为单向永久标记，配套 user_auth_epoch；所有 grant、operation/request alias、audit 记录引用 user_id 并记录当时 binding_id/generation。历史行不随新 binding 改写，查询不得把现任主体标签回填成历史操作者。

这里仍是一个 canonical Principal/user_id。绑定表是其身份历史，不是第二套权限模型。跨 organization 迁移、不同自然人账号合并、把 retired subject 重新分给他人，均不在此版许可范围；使用新 user_id 和独立授权，不继承旧历史/票据。

## 3. 永久保护与普通关联判定

以下任一成立即永久保护：曾被授予平台 grant（含撤销/失效）、作为平台 grant 的用户授予者/撤销者、作为 revocation operation 的 initiator/alias actor，或是 user/member/key/device 目标关联用户。对所有撤销历史保守保护，避免普通邮箱链接移交 ADR-0030 的原 initiator GET 权。这是有意选择的保守兼容边界：一个从未拥有平台 grant、也不是 operation initiator/alias actor 的普通员工，只要曾作为自己 Key 的撤销 target 关联用户，就永久失去 TASK-023 的自动同 namespace 邮箱关联资格。仅撤销一个 Key 也不例外；“普通用户保留”仅指尚无上述保护历史者。以后相同可信邮箱对应新 subject 时，普通登录返回 identity_binding_conflict，不改原 user_id/binding，不签新 session；确为同一自然人须走 §5 的人工受控迁移（旧身份不可用时走其 recovery），不同人邮箱复用不得继承旧账号。保护不因 grant 删除、任务完成、audit 归档而解除；新 operation/grant writer 在同事务设置标记。既有历史由迁移盘点回填，缺证据视为需隔离，不能按“当前无 live grant”放行。

普通入口 `ResolveOrLinkVerifiedIdentity`（替代所有裸 UpsertUser 写入）按同一事务执行：

1. 验证可信 namespace/组织及断言来源，先查精确 binding。已存在 current binding 仅更新允许的显示属性；disabled 不复活、组织不改、epoch 不回退。邮件若冲突到另一个 user，返回 identity_conflict，绝不移动任一身份。命中 retired binding 拒绝登录，转受控恢复。
2. 无精确 binding 且 email 为空：可 JIT 新普通 user/binding，不制造伪邮箱。未知 legacy 绑定冲突必须先迁移，不用新建行绕开。
3. 无精确 binding 且匹配现存邮箱：双方同组织、同可信 namespace、现存 user 未保护、未迁移/禁用，才保留普通目录关联：锁定用户，退役旧 binding，建立新 binding，推进 user_auth_epoch、使旧会话/票据失效并写关联 audit。不同 namespace 必须受控迁移；相同原始 subject 字符串也不例外。双方已各自对应不同 user 时拒绝，不能合并两行或挪邮箱解冲突。
4. 若邮箱命中受保护 user，返回通用 identity_binding_conflict；零绑定/grant/operation 改写、零新 session。不能临时撤销 grant、清保护标记或创建同邮箱别名绕过。缺邮箱的新主体只能成为新普通 user，永不因姓名/邮箱后补取得旧权限。

普通同 namespace 关联保留 canonical user_id 及 TASK-025 之后的 Workspace 同步，但旧身份的 session/JWT/device/key/download/同步票据不可带入新 binding：凭据必须绑定 binding generation/user_auth_epoch，在线拒绝旧值；legacy 无 lineage 的凭据拒绝。旧 key/device 终止使用后必须由新可信登录重新申请，不自动转发凭据。若有已登记执行则关联进入受控 pending，待旧执行按 ADR-0030 达到停止证明后才激活新 binding；已出现 operation 历史而保护的用户直接走 §5。无需终止的普通首次登录/资料更新仍同步完成。不能用退役 binding 的 token 调用 Profile 获得新 token。

## 4. 锁、受控 writer 与并发

TASK-065 将全部 OIDC/Feishu callback、dev-login、bootstrap receipt、用户导入/恢复、grant、operation/alias 和旧 UpsertUser 调用迁到一个 canonical writer 集合。runtime 非 owner role 不得直接更新 bindings、users.idp_subject/organization/protection/epoch。DB 约束/触发器必须阻止旧 writer 绕过，migration owner 仍是受控信任根；仅在新 helper 加判断不算防护。

已验证 IdP 断言的数据库提交还遵守 ADR-0033 §2.2：通用 `eap_gateway_runtime` 既不能直接 DML，也不能执行可自报 subject 创建/重绑身份或签 session 的受控函数；独立 `eap_identity_verifier` principal 仅由完成 IdP 验证的受控适配器持有，并被 DB 限定为明确的身份 writer。旧 `UpsertUser` 不得作为生产 callback/导入 fallback。凭据分离只能封闭通用 runtime SQL 凭据泄露后的冒名路径，不能把持有身份 principal 的人自报的函数参数称为数据库独立验证的 IdP 证据。

遵守 ADR-0033：installation authority 首锁；身份关联/迁移/保护/平台 grant 变更以 UPDATE 模式进入，禁止 SHARE→UPDATE 升级。随后相关 user 按稳定 ID、binding/邮箱候选、grant/member/key/device、execution/operation/request-id。新 user 无行可锁时仍由 installation 锁串行化；unique indexes 是额外防线。不以首次 SELECT 邮箱结果作为最终授权，全部锁后复查精确 namespace/binding、保护标记、当前邮箱与 epoch。网络 IdP 验证在事务外完成，receipt 在事务内复查期限并单次消费。不得锁着 DB 等 IdP、远端取消或 C。

grant 的授权对象不能只是 user_id：提交时必须匹配批准/receipt 中的 expected binding_id/generation/namespace/subject。若普通邮箱关联先提交，针对旧主体的授予/首引导因 stale binding 失败；若 grant/operation 保护先提交，邮箱关联失败。这样“邮件登录先赢锁”不会使新主体继承一个随后才提交的旧主体审批。任何高权限请求/R 将 actor binding/epoch 和 session lineage 在最终锁后一起复查。两 Gateway 不依赖缓存 invalidation 保证正确。

## 5. 受控高权限身份迁移

目的仅限已确认同一自然人的目录/IdP 主体变化；不是账户出售、邮箱复用或平台权限转让。所有 live/历史 grant/operation 用户、跨 namespace 普通用户都适用此流程。不同自然人必须新 user_id，即使组织重新分配了同一个邮箱。

受控维护通道（非公共 callback、非 Workspace admin）提交持久 UUID migration_id、精确 old/new namespace+subject、user_id/current binding generation、安装组织、原因/工单、操作者身份和同一自然人的独立目录证据。正常路径要求 old/new 两个近期 IdP 验证 receipt + 受控操作者确认；旧身份不可访问时可走 recovery，但须两名不同受控维护操作者独立批准及可信企业目录的不可变人员标识/变更记录，email 本身绝不算证据。不能验证同一自然人则 no-go。新主体 receipt 必须通过 identity-proof-only 流程取得，先验证 IdP 再生成待认领 receipt，不调用普通 UpsertUser；receipt 不创建登录权限或把目标 user_id 暴露给申请者。该流程供 ADR-0033 bootstrap 同样复用，绑定精确 namespace/generation。

状态机：`prepared → quiescing → ready → committed`；证据不足/远端未知为 `unresolved`，激活前取消为 `aborted`。准备记录不改绑定。进入 quiescing 同事务封闭 user 新动作、推进 epoch、撤销所有旧平台 grant/key/device/session/票据、登记相关 user R 和可靠 audit；最后管理员采用 ADR-0033 正常保护或维护 emergency 规则，不能暗中绕过。此后目标新旧身份均不能正常登录，后台停止继续；没有 C 不得进入 ready。无法证明旧远端执行终止时保持 unresolved，运维可隔离，但不能声称已停。

ready 后受控 commit 再核对未过期且未消费的证据/双批准、目标仍为预期 binding/epoch、new subject 未绑定他人、所有 R/C 证据与 audit。旧 binding 退役、新 binding 激活、user 重新启用并推进新 epoch、迁移审计同事务。旧 grant 永不复活；如确需恢复管理权限，另按 ADR-0033 新 grant generation 审批，绑定新主体。旧 key/device 不恢复。Workspace memberships 不转给不同人；同人迁移后仍须当前 IdP group 同步和在线检查，旧 session 快照不赋权。

迁移后的同一自然人可按 ADR-0030 以同一 canonical user_id 查询其历史最小状态；audit 显示历史 binding 与迁移链。不同人不得进入此流程，所以不能继承原发起人保留权。grant/审计历史不改名为新主体的行为。aborted 不撤回已提交的 R、不复活旧凭据/旧 grant；恢复旧主体也必须受控新 epoch 重新启用并重新认证，不靠 SQL 回滚历史。

## 6. 失败、审计与迁移上线

每个关联/迁移阶段以 UUID 幂等，绑定 operator/verified identity、old/new binding、版本和语义；同 UUID 改内容 conflict。身份变更、protection、epoch、receipt 消费与成功 audit 原子提交。审计不可写则不改变身份、不签 session；commit-unknown 保持 outcome_unknown，禁止另换 ID 重做。受限维护查询按持久 ID/当前 operator 权限恢复；不能给尚未证明身份的 callback 开放历史记录。拒绝记录稳定 reason_code/trace、opaque user/binding IDs 和受控证据引用，不记录 token、完整 email、IdP assertion；审计写失败有可观测告警且登录失败，不降级成功。

前向部署步骤：

1. 保持平台控制 session/公开 user R 关闭，盘点所有身份 writer、现存 users、grant/operation/audit/session/key 和配置来源。000023 只是参考实施，不能认为 FK 已解决 subject 重绑。
2. 受控生成 namespace 清单及 legacy binding 映射，证据来自固定配置历史与可靠 IdP/部署记录。不能按 email、subject 字符串前缀、当前默认 issuer 猜旧 namespace。无法证明的用户置 migration-required，封闭登录及相关凭据；已有执行未知不报 C。保留原行，人工恢复。
3. 在事务中回填不可覆盖 binding 历史/永久保护、安装受控 writer/DB privileges。旧 JWT/session/key 无 binding lineage 一律失效，要求可信重新登录/新 key；不能为兼容将旧 user_id token 补成当前 binding。双写过渡只能由同一受控事务维护 compatibility column，禁止两个权威源。
4. 切换全部 callback/Profile/Bearer/数据面 credential 查验，隔离排空旧 Gateway并撤销旧 writer DB 权限；关闭会自动走旧 email UPDATE 的兼容 fallback。执行 §7 真 PG/OIDC 双实例矩阵后才允许普通新路径；平台入口还需 ADR-0033/0030 全部门禁。
5. 回滚仅关闭新入口/写入，保留历史/保护标记；不能恢复旧 UpsertUser DML、旧 session/key 或取消 subject namespace 唯一约束。真实 IdP 合并/生产 grant 均非本设计卡行为。

## 7. TASK-065 可执行验收矩阵

| 场景 | 双实例/故障断言 |
|---|---|
| 普通 subject-only、email 后补、同 namespace 可信邮箱关联 | 原有 JIT/关联可用；相同 subject 幂等；保留 user_id；旧 lineage 全拒；不会伪称 email_verified |
| live grant/已撤 grant/历史 operation 用户同邮箱新主体 | 所有旧新 callback/Upsert/导入拒绝；原 binding/grant/历史不变、零 session |
| 仅普通 Key 撤销 target，既无任何 grant 又非 initiator/alias actor | 仍永久保护；同 namespace 同邮箱新 subject 返回 identity_binding_conflict，零 session/绑定变化；同人只可按 §5 维护迁移，不因“普通员工”放行 |
| A grant/bootstrap 批准旧 binding，B 新主体关联先提交/反序 | 真 PG 屏障：关联先则 stale binding grant 拒绝；grant先则关联拒绝；无管理员身份转移 |
| A 创建 operation/alias，B 同邮箱关联 | 共享 installation 锁；保护先则拒关联；关联先则旧 session epoch 被拒，不能提交旧 actor R |
| 两个新 subject 同邮箱；已有两个 user；namespace 同字符串 | 唯一 canonical 行、不合并已存在用户；串行同 namespace 普通关联明确退役旧 binding，旧凭据不再有效；不同 namespace conflict |
| Feishu open_id→union_id、不同 app/tenant/标准 issuer | namespace 不混淆；受保护/跨 namespace 必须维护迁移；unknown legacy no-go |
| 普通跨 issuer 同 email、新员工复用离职管理员 email | 不自动链接；同人受控迁移；不同人新 user_id，不承接 grant/历史 GET |
| 维护正常双 receipt、旧 IdP 不可用 recovery、伪造/重放 receipt | 只可信 same-person 证据+规定审批可推进；超时/重放/actor与binding漂移拒绝 |
| quiescing 双 Gateway 在途、remote_unknown、最后管理员 | 零新动作；无 C 不激活 new binding；emergency 有独立授权审计；不自动恢复 grant |
| 旧 cookie/JWT/agk/device/download ticket、Profile 重签 | 无 lineage/旧 epoch/retired binding 全部拒绝；新 IdP 登录后仍在线验证 grant/member |
| audit 失败、提交断连、重复 migration_id、取消/恢复 | 无部分绑定/receipt 消费；按持久 ID 查明结果；已提交 R 不反转；历史 immutable |
| 旧 owner runtime/直接 DML/漏迁 Gateway | 部署 no-go；非 owner role DB 拒绝 bypass；不能仅凭 Go helper 单测通过 |
| 通用 runtime SQL 凭据直调身份 writer、身份 writer DSN 误配为普通/owner、伪造 subject/重放 flow | DB 拒绝直调；Gateway 启动拒绝误配；真实受控适配器仅能在可信 namespace/安装组织和单次 flow 下原子建绑定、同步 group、写 audit 与签 session，失败零 cookie/绑定变化 |

最低 Store seam：`ResolveOrLinkVerifiedIdentity(verifiedEvidence, expectedBinding)`、`ProtectIdentityAndMutateAuthority(...)`、`Prepare/Quiesce/CommitIdentityMigration(...)`；名字可适配 Go，原子/权限边界不可拆。TASK-065 唯一负责 schema、trigger/role、Store、OIDC/Feishu/Profile/Bearer 和回归实现。本卡不改运行文件，不引入第二 owner。

## 8. 审查与剩余门禁

独立审查人：Sol `/root/wiki_boundary_review`。2026-09-25，root 转达其对固定 `474c8bb85b3feae3a88950b535a5446784feb2ef` 的初审：核心安全设计可接受，集成前须明确普通 Key 撤销 target 也永久失去自动邮箱关联资格的兼容成本。§1/§3/§7 随后明确这一有意保守取舍：即使没有 grant、不是 initiator/alias actor，也返回 identity_binding_conflict，确为同人仅可维护迁移/recovery。

同一审查人对修订固定 `46748095576576357d24a766521eeb4ddfc4d17e` 确认：兼容成本、拒绝/同人迁移路径和回归矩阵均已明确，核心规则未变，可以 Accepted。接受范围仅为 ADR-0020/0033 的有意优先级、主体绑定/永久保护、受控迁移与实施边界设计；不表示当前 Gateway/Store 已实现。

TASK-065 仍是唯一运行 owner。数据库/IdP 真实行为尚未实施验证；在可信 namespace 回填、canonical writer、credential lineage 及 ADR-0033 全部门禁前，平台管理员控制 session 与 user-target 公开撤销保持 fail closed。真实外部远端强停止仍需 ADR-0030/0032 的独立执行/终态证明。本卡不执行生产身份迁移、不授予 grant；集成与任务卡完成由 root 处理。
