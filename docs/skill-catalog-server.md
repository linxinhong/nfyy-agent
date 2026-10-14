# 技能目录与能力包分发

2026-10-12 起，技能目录及能力包分发由 [`server/backend/internal/catalog`](../server/backend/internal/catalog/) 提供，与中心身份、内容 API 共用 Go 服务。网站位于 `server/web/`；桌面 Runtime 通过 `YUANPU_CATALOG_URL` 连接同一服务，未设置时使用中心地址。服务启动与部署见 [Server README](../server/README.md)。

## 接口

- `GET /v1/catalog/search?q=<query>`：搜索公开目录，返回 `schemaVersion` 与条目列表。
- `GET /v1/catalog/items/<id>`：读取条目详情。
- `GET /v1/capability-packages/builtin.python.echo/manifest`：读取已经签名的 Python 示例能力包 manifest。
- `GET /v1/capability-packages/builtin.python.echo/artifacts/<filename>`：下载 manifest 中列出的平台归档。

目录仍由服务端静态定义；当 `YUANPU_ARTIFACT_ROOT` 指向包含有效 `manifest.json` 的目录时，示例能力的版本跟随 manifest。没有配置该目录时仍能搜索和查看条目，但制品端点返回 404。服务只允许发布规定文件名且在 manifest 中列出的普通归档文件，不暴露发布目录中的其他文件。客户端安装器验证签名、大小和 SHA-256，服务不会让目录响应执行任意安装命令。

生产服务仍需发布者身份、不可变制品存储、撤回状态和分页；详见 [中心服务待办](todos/yuanpu-server.md)。
