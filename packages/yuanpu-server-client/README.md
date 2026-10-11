# @yuanpu-agent/server-client

Yuanpu 中心服务的 TypeScript SDK 骨架，适用于具备 Fetch、AbortSignal.timeout/any 的环境。桌面应用应在 Runtime/宿主中调用，并经既有 preload 边界向界面提供业务能力。

```ts
import { createServerClient, ServerApiError } from '@yuanpu-agent/server-client';

const client = createServerClient({
  baseUrl: 'http://127.0.0.1:8788',
  timeoutMs: 10_000,
});

console.log(await client.health());
try {
  console.log(await client.ready());
} catch (error) {
  if (error instanceof ServerApiError) {
    console.error(error.code, error.status, error.requestId);
  } else {
    throw error;
  }
}
```

- 可注入 `fetch` 供宿主适配；每次调用可传 `{ signal }` 取消请求。
- 默认超时 10 秒，不自动重试或跟随重定向。
- `baseUrl` 可含反向代理路径前缀，不能包含用户名、密码、查询参数或 fragment。
- HTTP 业务错误由 `ServerApiError` 表示；网络、超时和取消错误保留底层错误。
- SDK 校验健康/就绪响应的必要字段，拒绝将旧目录服务或错误协议版本当成中心服务。
- 账号 API 尚未实现；当前没有登录、令牌存储或认证能力。

`src/schema.gen.ts` 来自 [OpenAPI](../../server/api/openapi.yaml)，不要手改。运行 `pnpm generate:server` 同时更新 Go 和 TS 类型，再执行 `pnpm check:server`。
