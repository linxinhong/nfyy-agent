# 技能目录服务

2026-10-11：同目录新增独立 Go 中心服务骨架，默认端口 8788；本文描述的 Node 目录服务继续运行在 8787。当前尚未迁移目录与制品 API。新服务入口与开发命令见 [Server README](../server/README.md)，演进边界见 [中心服务待办](todos/yuanpu-server.md)。

根目录 `server/` 是 YuanpuAgent 的最小技能目录服务。它负责搜索与技能元数据，不参与本机 Runtime，也不执行安装命令。

## 本地运行

```bash
pnpm dev:catalog
```

默认监听 `http://127.0.0.1:8787`。桌面开发流程会自动启动该服务，并通过 `YUANPU_CATALOG_URL` 交给 Runtime。

## API

- `GET /v1/health`
- `GET /v1/catalog/search?q=<query>`
- `GET /v1/catalog/items/<id>`

目录条目给出精确的 npm/Git 来源、展示名称、组件和权限。客户端安装器仍会固定版本、关闭生命周期脚本并验证真实 Pi 资源。

## 后续服务化边界

生产服务需要在当前只读目录合同上增加发布者身份、不可变制品、SHA-256、签名、兼容平台、撤回状态和分页。制品服务与搜索索引可以独立扩展，但不能让目录响应携带需要客户端执行的任意安装命令。
