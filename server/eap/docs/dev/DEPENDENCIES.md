# 依赖与许可证清单

> 任务卡 TASK-006（M0-M3 门禁）要求的基础依赖盘点。完整依赖图以
> `go list -m all`（Go 侧）与 `pnpm list --depth Infinity`（web 侧）为准。

## Go 直接依赖（go.mod，钉版）

| 模块 | 版本 | 许可证 | 用途 |
|---|---|---|---|
| github.com/maximhq/bifrost/core | v1.8.4 | Apache-2.0 | LLM Provider/协议执行内核（ADR-0004） |
| github.com/golang-jwt/jwt/v5 | v5.2.1 | MIT | 内部 JWT JOSE 实现（签发/验签） |
| github.com/jackc/pgx/v5 | v5.10.0 | MIT | PostgreSQL 驱动/连接池 |
| github.com/redis/go-redis/v9 | v9.22.0 | MIT | Redis（限流计数、就绪探活） |

Go 标准库承载 W3C traceparent、Ed25519、crypto/rand 等，无第三方加密/追踪依赖。

## Web 直接依赖（apps/web，脚手架 xingding-ui-cli 生成）

| 类别 | 组件 | 许可证 |
|---|---|---|
| 框架/构建 | React 19、Vite 8、TypeScript 5.9、Tailwind CSS 4 | MIT |
| 路由/数据 | TanStack Router、TanStack Query | MIT |
| 组件 | Radix UI 套件、cva、lucide-react、sonner | MIT |
| 测试 | vitest、Playwright | MIT/Apache-2.0 |

脚手架本身（xingding-ui-cli）是生成工具，不是运行时依赖（ADR-0003）。

## 复用参考仓库（非依赖）

| 仓库 | 许可证 | 处置 |
|---|---|---|
| ../bifrost | Apache-2.0 | 以钉版依赖采用 core 模块；升级随 tag + 回归门禁（ADR-0004） |
| ../xingding-llmg | 私有 | 仅参考；runtime 退役（ADR-0004 处置表） |
| ../xingding-mcpa | 私有 | 治理语义适配来源；内核未发布不可依赖（ADR-0005） |
| ../xingding-ui-cli | 私有 | 一次性生成 apps/web（ADR-0003） |

## 升级与再盘点

- 依赖变更须更新本文件；bifrost 升级额外运行 `make diag-upstream-race`（ADR-0004 已知问题）。
- 生产发布物需附 Apache-2.0/MIT 归属声明；bifrost 的 MPL-2.0 嵌入源码条目随其 THIRD_PARTY_NOTICES 传递。
