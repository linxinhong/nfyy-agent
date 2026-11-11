# 内置 Tailscale（macOS）

南医小助的 macOS 安装包自带 `tailscaled` 和 `tailscale` CLI（上游 `tailscale.com` v1.104.5，打包时用 Go 从源码编译，模块由 Go 校验和数据库验证）。用户在 **设置 → Tailscale** 输入一次 Auth Key，App 就会以独立设备加入 Tailnet，无需安装 Tailscale.app，也无需系统扩展或管理员权限。

## 工作方式

- **userspace networking**：`tailscaled --tun=userspace-networking`，不创建 TUN 网卡，不改系统路由和 DNS。它只对南医小助自己可见，其他应用不受影响。
- **生命周期**：Runtime 启动时，如果已保存 Auth Key，就启动守护进程；Runtime 退出时停止它。异常退出后自动重启，60 秒内最多 3 次。Runtime 若崩溃，留下的守护进程会在下次启动时按 pid 文件回收。
- **状态**：存放在 `~/.yuanpu/app/connections/tailscale/`，包括 `state/`（设备密钥和登录状态）、`tailscaled.sock`、`tailscaled.log`（超过 5 MB 时截断）。登录后重启不再消耗 Auth Key。
- **Auth Key**：只保存在 `tailscale.json`（0600）。传给 CLI 的方式是 `--auth-key=file:` 指向一个临时 0600 文件，用完即删，不会出现在命令行参数、日志或接口响应里。更换密钥时先 `logout` 再重新加入；移除密钥时 `logout` 并删除设备状态。守护进程带 `--no-logs-no-support`，不向 Tailscale 上传日志。
- **路由**：Runtime 在 Node 内置 `fetch` 前装了一个选择性 dispatcher。只有以下目标经由 tailscaled 的回环 HTTP CONNECT 代理：
  - `100.64.0.0/10`、`fd7a:115c:a1e0::/48`
  - 当前 Tailnet 的 MagicDNS 后缀（`*.<tailnet>.ts.net`），以及对端主机名
  - 对端发布的子网路由（默认路由 `0.0.0.0/0` 除外，不使用出口节点）

  其他请求照常直连。未连接时没有任何请求走代理，所以 tailscaled 出问题不会影响公网模型网关。子网路由只匹配 IP 字面量，解析到子网内地址的普通域名不会被代理。

## 推荐的 Auth Key

在 Tailscale 后台创建 **Reusable + Tagged + Pre-approved** 的 Auth Key，并用 ACL 限制该标签能访问的网关。不要把 Auth Key 写进安装包或仓库。

## 开发与验证

- 打包：`apps/desktop/scripts/prepare-tailscale.mjs`（非 macOS 输出空目录；设置 `YUANPU_TAILSCALE_SKIP=1` 可跳过）。产物在 `apps/desktop/dist-tailscale/`，放进 `南医小助.app/Contents/Resources/tailscale/`，随 App 签名和公证。
- 冒烟：`node apps/desktop/scripts/smoke-tailscale.mjs <目录>` 会启动守护进程，检查本地 API 返回 `NeedsLogin`、代理监听、SIGTERM 能退出。CI 的 macOS 打包任务会执行它。
- 开发态：`YUANPU_TAILSCALE_ROOT` 指向含两个二进制的目录；`YUANPU_TAILSCALE_DISABLED=1` 关闭。
- 测试：`apps/runtime/test/tailscale-node.test.mjs`，用假二进制覆盖生命周期，用回环 CONNECT 代理覆盖选择性路由。
