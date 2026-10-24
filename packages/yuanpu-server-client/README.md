# @yuanpu-agent/server-client

Yuanpu 中心服务的 TypeScript SDK，适用于具备 Fetch、AbortSignal.timeout/any 的环境。桌面应用应在 Runtime/宿主中调用，并经既有 preload 边界向界面提供业务能力。

```ts
import { createServerClient, ServerApiError } from '@yuanpu-agent/server-client';

const client = createServerClient({
  baseUrl: 'http://127.0.0.1:8788',
  timeoutMs: 10_000,
});

console.log(await client.health());
console.log((await client.wikiPreview()).entries.map((entry) => entry.title));
console.log((await client.searchCatalog('Python')).items.map((item) => item.displayName));
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
- `login()` 返回 bearer token；由宿主安全保存并创建带 `token` 的客户端实例。SDK 不持久化令牌，也不自动续期。
- 成员和团队管理使用 `listUsers/createUser/updateUser` 与 `listTeams/createTeam`；权限由服务端判定。
- 资讯、Wiki、AI 能力的草稿与审核使用 `listResources/getResource/createResource/editResource/submitResource/approveResource/rejectResource/withdrawResource/reviewQueue`；这些方法尚未接入正式网站和桌面界面。
- `wikiPreview()` 只读取公开示例词条，不是正式组织 Wiki 读写接口。
- `searchCatalog(query)` 与 `getCatalogItem(id)` 读取 Go 中心的公开技能目录；能力包安装仍由桌面 Runtime 完成。

`src/schema.gen.ts` 来自旧中心的 [OpenAPI](../../server/backend/api/openapi.yaml)，不要手改。旧中心维护时运行 `pnpm generate:server:legacy`，再执行 `pnpm check:server:legacy`。当前知识服务中心的契约位于 `server/eap/api/mindlink-openapi.yaml`。
