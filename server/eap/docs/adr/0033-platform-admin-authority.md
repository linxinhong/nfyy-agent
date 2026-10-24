# ADR-0033：平台管理员授权与全局撤销控制面边界

- 状态：Accepted（仅设计；Gateway 尚未实施，生产门禁不变）
- 日期：2026-09-25
- 任务：冻结平台管理员授权与全局撤销控制面边界（TASK-068）
- 运行实现唯一 owner：TASK-065；本 ADR 不提供生产授权或上线许可
- 上游：PRD §4.2–4.4；System Architecture §4；ADR-0030 §7.1；ADR-0032

## 1. 已核实事实与决定范围

`apps/gateway/admin.go:requireAdmin` 调用 `skills.go:requireControlPlane` 后检查 `workspace_members.roles` 的 admin。后者从 session 的 Workspace 起步，可能回退到首个 membership；它没有平台授权源。`migrations/000002_identity.sql` 的 users 有 organization_id，没有平台管理员字段；Workspace 有 organization_id，membership 主键是 workspace_id/user_id。`000012_group_role_mapping.sql` 和已完成 TASK-025 将 IdP groups 映射到 Workspace roles，登录时 additive union，不能把这种 admin 提升为全局授权。开发 seed 只有演示组织/Workspace，不能证明生产管理员资格。

PRD 区分 Workspace Owner 与 Agent Platform Admin。本决定增加同一身份数据库内的 canonical 平台 grant；不建立第二种 Principal，不改变 TASK-025 additive 合同，不把 email、JWT roles、IdP group 名称、Workspace admin 或 dev-login 当作平台 grant。TASK-065 未公开 Store 方法及分支测试不是已部署授权。

此版限定单个受控平台安装对应一个已存在 organization_id。该 installation organization 由受控初始化固定，必须与 actor、目标用户和目标 Workspace 的 canonical organization 一致。多组织混合部署在单独设计前 no-go；不能用请求 organization/header 或选择第一个组织推断权限。这里的“全局”指这个平台安装的全部 Workspace。

## 2. Canonical 存储与受控 writer

以下是 TASK-065 要实现的逻辑合同，具体迁移编号由该 owner 分配：

| 记录 | 最低字段与约束 |
|---|---|
| platform_authority | 安装唯一行：installation_id、organization_id FK、revision、bootstrap_sealed、created_at；初始化后组织不可在线变更 |
| platform_admin_grants | grant_id UUID、installation_id、user_id FK、generation、granted_at、granted_by（user 或受控 operator）、revoked_at、revoked_by、reason_code、audit_id；每用户最多一个 live grant；重新授予新 generation，旧行不可覆盖 |
| grant mutation request | UUID request_id、actor identity、语义版本、action、target、期望 grant generation、结果和 audit_id；持久幂等，不能靠进程内缓存 |

有效平台管理员 = 有效 Gateway audience 的控制面 session + 当前未禁用的 canonical user + 同安装组织 + 当前 live grant。grant 不从 JWT 取值、不缓存，不设隐式到期或登录续租；用户禁用立即使 grant 无效。重新启用用户不得使旧 grant 自动复活：user R 同事务撤销该用户全部 live 平台 grant，重新授予必须新 generation。grant 不改变模型/MCP/Knowledge 的 Workspace membership 或数据面 Policy。

runtime 使用非 owner、非 superuser、无 BYPASSRLS 的 DB role；无上述表或授权 epoch 的 INSERT/UPDATE/DELETE 权限。只能 EXECUTE 受控 writer/read-release 接口。受控函数固定 search_path、验证所有参数、不接受调用方传入的“已授权=true”。函数 owner 与日常 runtime 分离。数据库 owner/迁移操作者仍是可信根，需要受控部署和审计；声明了函数而仍用表 owner 运行不算完成。

### 2.1 不依赖 Workspace 的控制面认证

现状缺口：`oidc.go:164` 在零 group bindings 时拒绝登录，且在该判断后才 UpsertUser；`requirePortalSession` 要求至少一个有效 membership，`handleOIDCProfile` 依赖它；Bearer `requireControlPlane` 同样回退查 membership。因此现有 session 链路不能承载“只有平台 grant 的管理员”，更不能作为首次 bootstrap 的身份验证证明。这些路径必须由 TASK-065 一起实施后才可开放本 ADR 能力。

新增明确的仅控制面 audience `eap-control-plane`，issuer 仍是现有配置的 EAP canonical signer/issuer、相同受控 JWKS 信任链，绝不直接接受 IdP token 或内部服务 JWT。Bearer 包含 sub、iss、唯一 aud、iat/exp、jti、canonical user epoch、server-side portal session ID；无 Workspace、roles 或 admin 权限声明。TTL 最多 5 分钟且不超过服务端 session 剩余时间；每次请求验证签名/issuer/单一 audience/时间，并在线查 session 未撤销未过期、user active/epoch/安装组织。服务端 session 的身份关系不可由请求替换。此认证只证明是谁；每次管理能力依 §4/§5 再读 grant/目标权限。

OIDC callback 将“可信身份验证”和“Workspace 能力映射”拆开：保留标准 OIDC code/state/PKCE/nonce/JWKS/issuer/audience 检查，或已有受控 Feishu server-side exchange/user_info 验证；仅接受部署固定 IdP tenant/issuer 到安装 organization 的可信映射，不能继续以硬编码 org_default 证明生产组织。将验证过的 issuer+subject 绑定精确 canonical user_id；本版单安装只允许一个受控 IdP subject namespace，换 issuer 必须显式迁移，不能因 email 相同合并账户。创建/更新身份不清除 disabled 状态、不授予 grant、不重置 user epoch。零 bindings 是有效的空映射，仍执行 TASK-025 的同步语义（撤除不再映射的 Workspace）；映射查询失败不能当空数组成功。

验证成功且 canonical user active、安装组织匹配时，即使零 membership/零 grant，也可创建身份专用 HttpOnly/Secure/SameSite session，保存可信 issuer/subject、user_id、user epoch、验证时间、期限和撤销状态，写可靠认证 audit 后才发 cookie。它不是管理员资格。首次 bootstrap 操作者只从受控 DB 接口取得该 session 对应的近期（最多 5 分钟）IdP 验证 receipt，核对精确 user_id/issuer/subject/organization/active；receipt 单次消费并绑定 bootstrap 工单及安装，不能用浏览器自报 subject、旧 JWT 或截图代替。操作者确认、receipt 消费、首 grant、bootstrap seal 和 audit 同事务。没有 grant 的身份可完成验证，仍不能自行 bootstrap。

Profile 接线：抽出仅验证上述身份 session 的 `requireIdentitySession`，供 OIDC Profile/控制 token issuance 使用；Workspace 页面保留原 `requirePortalSession` 的 membership 要求。Profile 允许 `workspaces: []`、`default_workspace: ""`，新增 `control_session_token`（上述 audience），只提供最小用户资料和当前能力提示；提示不是权限。旧 `session_token` 仅在现有 Workspace 条件满足时签发，否则为空字符串，Portal 必须理解“无 Workspace 的控制面状态”，不得自动选默认 Workspace。Profile 与 token issuance 均 no-store、可靠 audit、在线检查 session/user，cookie mutation 仍有 CSRF/Origin 防护。

Bearer 接线：新增专用控制面 verifier，四目标撤销/GET 和 grant 管理命令使用它；既有 `requireControlPlane` 留给 Workspace 能力，不可通过跳过 membership 把新 token 接入 Skill 发布等路径。`eap-control-plane` audience 在 `/v1/*`、`/mcp/*`、Knowledge、Skill/Agent 内容下载和 key 数据面鉴权全部拒绝；agk/device 数据面仍须 canonical Workspace membership/Policy，平台 grant 无豁免。Portal 新控制操作只用 `control_session_token`；旧 agent-gateway token 不作为本版新入口认证 fallback。ADR-0030 的“有效控制面 session”由此具体化，operation wire 不变。

### 2.2 已验证 IdP 断言到数据库 writer 的信任边界

OIDC/Feishu 的外部验证在 Gateway 的受控适配器中完成，PostgreSQL 不能把函数参数中的 `issuer/subject/email` 当成验证证据。若可由通用 `eap_gateway_runtime` SQL 登录直接调用创建身份或会话的 `SECURITY DEFINER` 函数，取得该 SQL 凭据者即可自报任意已绑定 subject 冒充员工。因此生产身份 writer 不授予该通用运行角色；只在应用层检查“已验证”也不能弥补这个权限缺口。

同一 Gateway 中建立独立的身份验证 DB principal `eap_identity_verifier`，由受控 OIDC/Feishu 回调组件使用与 `EAP_DATABASE_URL` 不同的服务端凭据。它是信任根：只有持有其独立凭据的受控适配器可以向受控 writer 提交已完成 code/state/PKCE/nonce/JWKS/issuer/audience 或 Feishu server-side exchange/user_info 验证后的断言。此角色不得继承 owner/通用 runtime 权限，不拥有表/函数，不可 `SET ROLE`、建对象或直接 DML；只可执行明确列出的 IdP flow 消费、canonical 绑定/Workspace group 同步/身份 session 与认证 audit 原子 writer。通用 runtime 仅可调用不赋予身份的预登录 flow 创建和受限在线读取接口。函数固定 `search_path`、验证部署固定 namespace/安装组织/flow 期限与单次消费，从 canonical 记录推导 user/epoch/binding，不接受调用方传入 user_id、roles、已授权标记或组织选择。身份提交、flow 消费、group 同步、session 与可靠 audit 必须同事务；任何失败不签 cookie。

部署必须独立提供和轮换该凭据，不能把 owner DSN、签名私钥或普通 runtime DSN 复用为身份凭据；不写入员工配置、日志或响应。启动时核验两个 DB principal 的实际权限及不相等关系；配置了 OIDC/Feishu 而缺少可信 namespace 映射或受限身份 principal 时直接拒绝启动/关闭该登录入口，不能回退旧 `UpsertUser`、硬编码组织或 owner 连接。此边界防止仅泄露普通 runtime SQL 凭据的冒名；完整 Gateway 进程或身份 principal 凭据失陷仍属于高权限事故，需隔离入口、轮换凭据和撤销受影响 session/Key，不能声称数据库自行验证了外部 IdP 密码学证明。若部署要求抵御可信适配器进程失陷，需另行评审隔离验证器，而非扩大此 writer 的声明。

## 3. 引导、授予、吊销与恢复

生产默认零 grant、bootstrap 未封存、相关全局管理入口关闭。首次引导只允许部署操作者在独立维护通道调用受控 bootstrap：核对 installation organization、已完成可信 IdP 登录的精确 user_id 与 idp_subject、用户 active，以及明确变更工单/operator identity。不得按 email 猜用户、自动建默认账户或提供公共 bootstrap endpoint。事务锁 installation 行，要求无历史 bootstrap，创建第一条 grant、封存 bootstrap、写可靠审计后提交；失败全部回滚。operator 凭据仅在维护通道，不能由 Gateway request 构造。缺少可验证 operator identity/审计接入时保持 no-go。

后续授予/撤销只允许当前平台管理员，通过受控 Store 管理命令（本版不新增公开 grant 管理 HTTP 路由）。命令必须是该管理员的有效控制面 session，经相同在线鉴权，UUID request_id、精确 user_id、reason_code；撤销还需 grant_id/generation。授予对象必须 active、同组织；给已有 live grant 的同义请求为幂等结果。撤销不能因 user_id 重用误删后来重授的 generation。请求同 ID 改 actor/语义/目标报 conflict。重试也先验证当前 actor，丧失授权后不能凭旧 ID 获取敏感结果或重新操作。

正常 grant 撤销及 user R 不允许移除最后一名有效平台管理员：在 installation 锁内计算 active user + live grant 数，若将归零，返回 409 last_platform_admin，R 未发生、不返回 pending。自撤销/自禁用仅在另有有效管理员时可做。不能为了保留管理员而容忍已判定泄露的凭据：独立维护通道允许带 emergency 工单、可验证 operator identity 的紧急撤销最后一名，同时封闭全部公共平台管理入口；仍需 canonical mutation 与可靠 audit 原子提交。无可靠 audit 时不伪报成功，由部署操作者隔离 Gateway/外部出口，再恢复审计后执行。离线恢复以同样受控维护通道授予已验证 active 用户，不解封首次 bootstrap，不自动复活已撤销 grant。

维护操作保留 operator、工单、目标、前后 generation/revision、结果；不记录 token、email 原文、IdP assertion 或凭据。此维护能力不是额外公共 Principal 类型，不赋予普通 Gateway runtime。上述最后管理员规则只限制平台 grant/user 管理，不限制 Key/device 紧急吊销。

## 4. 四类目标与 GET 的授权矩阵

沿用 ADR-0030 §7.1 的版本头、202/204、Location、UUID 和错误合同，不新增第五类 revocation target。

| target | 当前有效控制 session 的最低权限 | canonical 检查 |
|---|---|---|
| key | key 所有人或平台管理员 | user/key 关联、同安装组织；Workspace admin 不授权他人 key |
| device_family | family 所有人或平台管理员 | family/user 关联、同安装组织；保留 ADR-0030 的 refresh-hash 窄 logout 适配器 |
| membership | 该 Workspace 当前 admin 或平台管理员 | actor active、Workspace 同组织、目标 user/member 对；其他 Workspace admin 无权 |
| user | 平台管理员 | actor/target active 状态与 grant 在线判定；已禁用目标允许有权管理员查询/重复撤销；最后管理员规则见 §3 |

用户自禁用也必须平台 grant；普通员工不能借 self 规则调用 user target。平台 grant 不依赖选中的 Workspace，不调用 requireAdmin 的 fallback 来获得权限。Workspace admin 分支只读取精确目标 Workspace 的 membership；不信 JWT roles。即使同一用户同时拥有多种角色，也记录本次实际满足的权限依据及 generation/epoch。

GET 每次先验证有效控制 session 和 actor 未禁用，然后满足“canonical operation 原发起人，或当前有该目标管理权限者”。原发起人失去 admin/member（包括最后一个 membership）后，仍可通过身份专用 session/Profile 取得仅控制面 token，读取自己已发起 operation 的最小状态（ADR-0030 的保留权限）；不能据此发起 mutation、读取他人审计或取得敏感目标详情。GET 只返回既有 operation 合同的 ID/状态/时间/通用原因，不附 payload、endpoint、内部 proof 或 credential。actor 被禁用后旧 session/旧 operation ID 均不能恢复查询；由另一当前有权管理员持自己的 session 接管。自禁用 commit-unknown 同理，不能为可恢复性放宽 disabled 检查。管理员接管需要知道持久 operation ID 或经授权以同目标新请求映射，不能依赖已失效发起人的 token。

跨 actor 规则：同一个 UUID 换 actor 仍 409（完成当前目标权限检查后）；不同有权 actor 使用自己的新 UUID 撤销同一 pending/unresolved 或已 completed 的 target epoch，映射到相同 canonical operation。alias 行绑定该次 request actor，canonical operation 保持最初 initiator，不能用复合 FK 强制两者相等。alias 所有权本身不增加 GET 权限；后来的 actor 丧失目标管理权限且非原发起人时 GET 为 404。actor 禁用后的相同 UUID mutation retry 拒绝认证，不能先查幂等结果绕过。以上区分“可能已提交”和“当前有权恢复”，不改变 ADR-0030 的 commit-unknown 语义。

目标不存在/无权统一 404；无效或禁用 session 401；权威库不可达 503，绝不使用旧角色兜底。所有响应 no-store；agk_*、内部服务 JWT、客户端 roles 不具备通用管理权限。

## 5. 并发、R 和可靠审计

所有 grant writer、user R、membership 缩权及这四类受保护的 R 使用统一次序：installation authority 行 → actor/target users 按稳定 ID 排序 → 相关 grants → 精确 membership/key/device authority 行 → execution/operation/request-id 行。先得全部必要锁，再在同一事务使用最终 DB clock 校验认证有效期和 canonical 权限并执行 R+audit。控制授权判断用 installation SHARE 锁；任何改变平台 grant/用户有效性/最后管理员计数的 writer 先取该行 UPDATE 锁，不允许持 SHARE 后升级。user R 从开始就取 UPDATE。membership 变更也必须遵循 authority-first 顺序。数据面 dispatch 不需要读平台 grant，但与 user/member R 共享已有 authority 行，遵循 ADR-0032 authority→execution 次序。禁止一条路径先锁 execution 再回头取 user。

授权变更先提交：等待中的管理动作重新检查，零 R、零 alias、零远端操作。管理 R 先提交：后来 actor 失权不撤回这个合法 R，后台停止和 C 用受限服务身份继续处理原目标，不依赖旧 actor token；不能恢复目标权限。新 grant generation 不得让旧请求在新 epoch 自动续行。网络调用、远端停止等待不持这些 DB 锁；R 请求有 ADR-0030 有限预算。

R、request/alias、目标 epoch、必要 grant 撤销、revocation.started audit 必须同事务；completion audit 与 C 同事务。grant 变更及 authority revision 的可靠 audit 也必须同事务。审计失败全部回滚，丢失提交响应返回 outcome_unknown，按持久 ID 恢复；不能把“审计服务收到”当作 DB 已提交。受控 Store 不接受上层已校验角色替代 DB 内检查。

GET 的在线查询和响应释放也要在 authority guard 下登记；释放前重验 actor active 以及原 initiator/当前管理依据，跨实例缩权使未释放响应 fail closed。已经发出的字节不可追回；未发出的尾部通过 TASK-065 writer join/fencing 停止。不得拿一个查询开始时的角色快照为慢响应无限续权。原 initiator 的最小状态保留权按 §4 处理。grant 管理命令的成功只声明 canonical grant 变更提交，不等同四类 revocation 的 C；所有涉及 user 强停止的完成仍仅在 C。

## 6. TASK-065 实施 seam 与切流

TASK-065 唯一负责 schema/controlled writers、Store 接口及所有调用点迁移；本卡不引入并行实现 owner。最低接口应表达 `AuthorizeAndBeginRevocation(session, requestID, target)`（同事务在线授权+R）、`ReadRevocationAndRegisterRelease(session, operationID)`、`MutatePlatformGrant(session, requestID, action, exactGeneration)`、独立维护 `Bootstrap/RecoverPlatformGrant(operatorEvidence, ...)`。名字可遵守现有 Go 风格；原子边界不可拆成先 CheckAdmin 后裸 Store.RevokeUser。非公开原始 Store mutation 不能被新 HTTP handler 直接暴露；受控函数也必须覆盖直接 DML 防护。

切流顺序：空表/封闭路由 → 身份专用 OIDC callback/receipt/Profile 与新 audience verifier、Portal 配套升级 → 验证非 owner role 与 bootstrap audit → 受控引导 → 双实例矩阵通过 → 授权适配层接入四类 R/GET → 再经过 ADR-0030/0032 全部执行覆盖和远端 proof 门禁才能打开对应公开 v2。缺任一门禁 user-target 始终 fail closed；不能因为其 Store 测试通过开放 endpoint。旧 requireAdmin 可以暂时保留既有 Workspace 管理语义，但绝不接入全局 user/grant 控制面；其他现有全局管理页需逐入口盘点迁移，在确认 canonical grant 前不得作为新全局撤销的先例。

混合版本不能让旧 Gateway 绕过新授权/受控 DML；部署须路由隔离并排空旧管理 writer，撤销旧 DB 写权限。回滚只关闭新路由/管理写能力，不回退到 Workspace admin 或 email allowlist；保留 grants、revision、operation 和审计。未有可验证引导操作者、跨组织模型、全实例受控 writer 覆盖或可信远端停止时保持对应 no-go，不伪称强停止已上线。

## 7. 必须实现的双 Gateway 回归矩阵

| 场景 | 必须观察的断言 |
|---|---|
| 零 membership/零 grant 的可信 IdP 身份 → receipt → offline bootstrap → control token | 可验证主体且只能操作者授予；新 token 可管理，数据面全部拒绝；错误 issuer/org/disabled/重放 receipt 均拒绝 |
| 已发起 R 后失去最后 membership/grant，A/B 分别 Profile/GET/新 mutation | active 原 initiator 可续取仅控制 token 并最小 GET；新 mutation 重新授权拒绝；数据面无 Workspace 拒绝；再禁用 actor 则 Profile/GET 都拒绝，由另一管理员接管 |
| secops admin 撤销 finance 用户；JWT 填 admin；dev email allowlist | 404/401；零 R、alias、grant 和远端取消 |
| key/device 本人、非本人 Workspace admin、平台 admin | 仅本人/平台 grant 成功；无 membership 仍可凭合法平台 grant 管理同组织目标 |
| membership 精确 Workspace admin / 其他 Workspace / 平台 admin | 仅精确 Workspace 或平台授权通过；数据面范围未扩大 |
| B 撤销 actor grant/member 或禁用 actor，A 在 R 锁前/等待中 | 真实双事务屏障；变更先提交则 A 无 R/audit-start；A 先提交则后续停止继续 |
| 最后管理员自撤销/禁用；两管理员并发互撤销 | 普通路径不归零；锁内计数；emergency 仅维护身份可归零且审计可靠 |
| 新授予、撤销、重新授予和旧 UUID/generation | 无自动复活；旧 generation 不能撤新 grant；登录 additive 不写 grants |
| 不同 actor 同目标不同 ID；同 ID 换 actor | 前者经当前授权持久映射同 canonical op，后者 conflict；canonical initiator 不变 |
| 自禁用提交丢响应；另一 admin 接管 | 旧 session retry/GET 拒绝，另一 admin 可查/映射；不新建第二个 R |
| 原发起人失去 grant/member；非原发起人 alias 后失权 | 前者 active session 仅最小 GET；后者 404；两者新 mutation 都重新授权 |
| audit INSERT 故障、commit-unknown、DB 断连、双 Gateway | 无部分 grant/R/alias；unknown 不伪报 rollback/C；恢复按持久 ID |
| 查询后缩权/禁用与慢响应 | 未发送部分停止；writer 有登记/join；旧 snapshot 不释放数据 |
| 直接 runtime DML、伪造 bootstrap、旧 Gateway、跨组织目标 | DB/入口拒绝；无 seed 默认 grant；没有第二条绕过路径 |

## 8. 审查记录和限制

独立审查人：Sol `/root/wiki_boundary_review`。初审固定 `6aa5fec3dac22566c45a326fbb4843631585fd2a` 的唯一 blocker 是 Workspace-free 控制 session/首次 bootstrap 无可信身份链；§2.1 及回归矩阵补齐该设计。其余最后管理员/emergency、disabled/跨 actor GET、锁序/审计/role 切流无独立阻断。

2026-09-25，root 转达同一审查人对固定 `1cb452d60f8d95ae8fe63b065611ab9df335e8fd` 的独立复审结论：初审 blocker 已闭合，ADR-0033 可标 Accepted，范围仅为平台管理员 canonical grant、Workspace-free 控制面认证与引导、四目标/GET 授权及实施边界的设计。此状态不表示现有 Gateway 已实施或获得生产上线许可。

TASK-065 仍是唯一运行实现 owner。本设计未执行生产引导、未授予任何权限、未实施 schema/API；公开 user-target 在 §6 的可信身份/grant、非 owner runtime、受控 writer 与双实例切流门禁实现并验证前 fail closed。强停止还需 ADR-0030/0032 的执行覆盖、可靠审计和真实远端终态证明；本次设计接受不解除这些生产 no-go。集成和任务卡完成由 root 处理。
