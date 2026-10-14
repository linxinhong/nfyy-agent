# @yuanpu-agent/word-editor

供 Yuanpu 使用的独立 TypeScript 文档接入层：Node 宿主管理本地编辑引擎，浏览器入口挂载 Word 编辑界面。支持打开、只读、修改、保存、另存为、未保存保护、查找替换和进程释放。

## 引擎依赖与交付范围

WorkBuddy 5.6.2 的完整 Word 编辑体验由 `@tencent/tencent-docs-ai-engine@0.2.165-wb` 的原生 `editor_sdk` 提供。该二进制包含文档模型、编辑界面、格式导入导出及本地 HTTP/MCP 服务。本包是根据观察到的协议独立编写的适配器，未包含 WorkBuddy 前端源码、原生引擎或厂商资源。

完整编辑需要调用方提供兼容的引擎绝对路径和固定 SHA-256。安装包里的 npm 元数据标注 MIT，但不能据此确认原生引擎的再分发权；正式发行应使用获得授权的引擎来源。公开 npm registry 未提供所观察到的引擎版本。

本机已验证 WorkBuddy 5.6.2 随附的 macOS arm64 引擎。Windows、Linux、其他引擎版本和历史 `.doc` 等格式尚未验证。验证用 DOCX 是本项目生成的合成文档，包含标题、页眉和表格。

运行不需要启动 WorkBuddy，也不需要把引擎保留在 `WorkBuddy.app` 中。可将有权使用的引擎导入 Yuanpu 自己的目录：

```sh
pnpm --filter @yuanpu-agent/word-editor import:engine /absolute/path/to/editor_sdk <pinned-sha256>
pnpm --filter @yuanpu-agent/word-editor smoke:native
pnpm --filter @yuanpu-agent/word-editor demo
```

导入脚本校验原文件和复制结果，将引擎放到 `~/.yuanpu/word-editor/engines/`，并写入 `~/.yuanpu/word-editor/engine.json`。演示与 smoke 默认读取此配置，之后不再引用 WorkBuddy 安装目录。该原生引擎仍是完整编辑功能的外部运行依赖。

## 使用

工作区依赖：

```json
{ "dependencies": { "@yuanpu-agent/word-editor": "workspace:*" } }
```

在 Electron main 或有文件权限的 Node 宿主中创建一个工作区对应的 host：

```ts
import { WordEditorHost } from '@yuanpu-agent/word-editor';

const host = new WordEditorHost({
  enginePath: config.wordEnginePath,
  engineSha256: config.wordEngineSha256,
  workspaceRoot: work.root,
});

const document = await host.open('reports/report.docx', { load: 'model' });
await host.replaceText(document.id, '旧文本', '新文本');
await host.save(document.id);
const nextDocument = await host.saveAs(document.id, 'reports/report-copy.docx');
await host.close(document.id);
await host.dispose();
```

在 renderer 中只导入浏览器入口，通过已有 preload IPC 取得宿主授权的 descriptor：

```ts
import { mountWordEditor } from '@yuanpu-agent/word-editor/renderer';

const document = await bridge.word.open('reports/report.docx');
const mounted = mountWordEditor(container, document, {
  locale: 'zh-CN',
  theme: 'light',
  onLoad: () => hideLoadingShell(),
  onDirtyChange: (dirty) => updateUnsavedIndicator(dirty),
  onSaveRequest: () => bridge.word.save(document.id),
});

// 使用 bridge.word.status(document.id) 定期同步真实 dirty 状态。
// 原始引擎版本可能不发送 WorkBuddy 自己的消息桥事件。
mounted.destroy();
```

`open()` 默认 `load: 'frame'`，由 iframe 导入模型，随后用 `status()` 确认就绪；导入前返回 `document_not_ready`。纯 Node 操作使用 `load: 'model'`，此时 `open()` 会等待模型导入。一个会话只能选一种导入方式：观察到的引擎在 MCP 先导入后再由 iframe 克隆模型时会产生会话 ID 错位。浏览器入口会拒绝挂载 `model` descriptor。`onLoad` 仅表示 iframe 页面加载，不表示首次内容绘制。浏览器入口没有 Node、React 或其他运行时依赖，React 项目可在 effect 中 mount/destroy。

## API 约定

| 方法 | 行为 |
| --- | --- |
| `open(relativePath, { mode?, load? })` | 返回含不透明会话 ID 的 descriptor；mode 默认 `edit`，load 默认 `frame`；纯 Node 使用 `model` |
| `status(id)` | 获取真实引擎的 dirty 和最近保存时间 |
| `findText(id, text)` | 返回引擎文档坐标；坐标不是字符串下标 |
| `replaceText(id, from, to)` | 重新查找坐标并替换，返回匹配数量 |
| `save(id)` | 保存当前文件，重新检查文件路径 |
| `saveAs(id, newRelativePath)` | 新建文件，切换后续保存目标，返回更新后的 descriptor；不覆盖已有文件 |
| `close(id)` / `dispose()` | 存在未保存修改时抛出 `unsaved_changes` |
| `close(id, { discardChanges: true })` | 显式放弃修改后关闭文档 |
| `dispose({ discardChanges: true })` | 显式放弃修改，终止所拥有的引擎并清理临时运行目录 |

同一个 host 的调用按顺序执行，避免保存抢在替换前完成。并发打开同一文件复用会话，宿主须确保每份文档只有一个活动 iframe。iframe 导入后，引擎会生成自己的 ID，本包只绑定与已授权文件路径一致的唯一 pool entry。另存为后原生引擎保留旧会话标识，因此再次打开原文件前需要关闭该会话。`mountWordEditor().destroy()` 只移除 UI，关闭文档仍由宿主处理。退出应用时先等待正在执行的操作，再保存或显式放弃修改。

文件路径必须相对工作区，不能越界或经符号链接指向工作区外。默认文件上限 64 MiB，可由宿主显式配置。SHA-256 不匹配时不启动引擎。请求有超时，进程停止先发 SIGTERM，超过期限后发 SIGKILL。

## Yuanpu 接入边界

当前仓库的 `apps/app/src/viewer/preview/file-preview.tsx` 仍使用已有预览类型。本次交付是可复用库和独立演示，尚未接入正式文件预览 UI，也没有更改 Runtime 的文件访问协议。

生产接入应保留 Work/workspace 授权：在 Electron main 或 Runtime 中注册会话，把 `open/status/save/saveAs/close` 作为窄 IPC 能力暴露给 renderer，校验所属 Work。`WordEditorDocument.url` 是有文件访问能力的引擎 URL，内部含原生绝对路径，不适合作为任意可分享链接。宿主层的路径验证约束本包调用；原生服务自身的 HTTP/MCP 接口及 iframe 是独立的信任边界，本包不提供操作系统沙箱或引擎 HTTP 访问认证。正式接入还需按 Yuanpu 的隔离模型安排引擎网络访问与文件权限。

## 本地验证与演示

调用方显式设置兼容引擎和固定摘要：

```sh
export YUANPU_WORD_ENGINE_PATH='/absolute/path/to/editor_sdk'
export YUANPU_WORD_ENGINE_SHA256='<pinned 64-character digest>'
pnpm --filter @yuanpu-agent/word-editor build
pnpm --filter @yuanpu-agent/word-editor test
pnpm --filter @yuanpu-agent/word-editor smoke:native
pnpm --filter @yuanpu-agent/word-editor demo
```

演示会创建独立临时目录及合成文档，输出浏览器地址和文件保存目录，支持界面编辑、保存、另存为、dirty 提示。退出演示保留已保存文件，关闭演示进程会放弃尚未保存的演示修改。原生 smoke 会验证修改、另存为、保存、重新导入后的内容、原文件未改变、只读拒绝和端口关闭；设置 `YUANPU_WORD_KEEP_FIXTURE=1` 可保留结果。

```sh
pnpm --filter @yuanpu-agent/word-editor pack --pack-destination /tmp
```

生成的 tgz 包含接入层、类型、合成测试文档与演示脚本，原生引擎由调用方另外提供。

## 验证记录（2026-10-11，macOS arm64）

- 本包 build/typecheck 及 10 项行为测试通过，包含独立引擎导入、真实请求超时、只读、保存顺序、路径边界和 iframe 会话 ID 绑定。
- 从 `~/.yuanpu/word-editor/` 中的引擎副本完成原生 DOCX 导入、查找替换、dirty 拒绝关闭、另存为、再次保存、关闭后重新导入、原文件保持不变和进程端口关闭检查。
- Safari 中加载独立演示，界面结构/格式修改触发 dirty；点击保存后重新检查 DOCX XML，确认新增段落写入文件。使用 LibreOffice 导出 PDF 并检查页面，标题、页眉和表格可见。
- `pnpm check` 的全仓库构建与类型检查通过；测试仍受 `packages/yuanpu-runtime/test/builtin-agent-context.test.mjs:58` 的技能排序断言失败影响。该用例单独运行也失败，本包测试在全仓检查中全部通过。
