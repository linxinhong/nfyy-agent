"""
WordBridge — 主入口
────────────────────────────────────────

MindLink ↔ Microsoft Word 后台桥接服务。

启动流程：
  1. 加载配置
  2. 初始化 COM Worker（STA 线程）
  3. 初始化 Region Registry（内存 + SQLite）
  4. 初始化 OoxmlEngine
  5. 初始化 WebSocket/HTTP 服务器
  6. 注册全局热键
  7. 运行服务

关闭流程：
  1. 注销热键
  2. 停止 WebSocket/HTTP 服务器
  3. 停止 COM Worker
  4. 清理过期的 Region
"""

from __future__ import annotations

import asyncio
import os
import signal
import sys
import time
from pathlib import Path

from .addin_worker import AddinWorker
from .com_worker import ComWorker
from .worker_base import CmdType, WorkerBase
from .config import Config, load_config, set_config
from .hotkey_hook import create_hotkey_hook
from .ooxml_engine import OoxmlEngine
from .region_registry import RegionRegistry, RegionStatus
from .utils import get_logger, setup_logging
from .ws_server import BridgeServer, WordBridgeService


class WordBridgeApp:
    """
    WordBridge 应用程序主类。

    管理所有子模块的生命周期。
    """

    def __init__(self, config_path: str = None, stdio_mode: bool = False):
        # 加载配置
        self.config: Config = load_config(config_path)
        set_config(self.config)

        # 日志
        if stdio_mode:
            # Windows 上 MindLink 可能按系统代码页读取 stderr。中文日志写文件，
            # stdin/stdout/stderr 均不混入协议之外的数据。
            stdio_log = str(Path(os.environ.get("TEMP", ".")) / "wordbridge-stdio.log")
            self.log = setup_logging(
                log_level=self.config.bridge.log_level,
                log_file=stdio_log,
                console=False,
            )
        else:
            self.log = setup_logging(
                log_level=self.config.bridge.log_level,
                log_file=self.config.bridge.log_file,
            )
        self.log.info("═══════════════════════════════════════")
        self.log.info("  WordBridge v%s 启动", self.config.bridge.version)
        self.log.info("═══════════════════════════════════════")

        # 子模块（延迟初始化）
        self.com_worker: WorkerBase | None = None
        self.registry: RegionRegistry | None = None
        self.ooxml_engine: OoxmlEngine | None = None
        self.service: WordBridgeService | None = None
        self.server: BridgeServer | None = None
        self.hotkey_hook = None

        # 信号处理
        self._shutdown_event = asyncio.Event()
        self._capture_callbacks: list = []

    # ── 初始化 ──

    def init(self, stdio_mode: bool = False) -> bool:
        """
        初始化所有模块。

        返回 True 表示初始化成功，可以继续 run()。
        """
        # 1. Region Registry
        self.log.info("[1/%d] 初始化 Region Registry...", 4 if stdio_mode else 5)
        self.registry = RegionRegistry(config=self.config.regions)
        recovered = self.registry.recover_from_db()
        if recovered:
            self.log.info("  → 从数据库恢复了 %d 个 Region", recovered)
        else:
            self.log.info("  → 初始化完成")

        # 2. OOXML 引擎
        self.log.info("[2/%d] 初始化 OoxmlEngine...", 4 if stdio_mode else 5)
        self.ooxml_engine = OoxmlEngine(config=self.config.ooxml)
        self.log.info("  → 策略: %s", " → ".join(self.config.ooxml.strategy_order))

        # 3. Word Worker（平台自适应：Windows=COM，macOS=加载项 WS）
        if sys.platform == "win32":
            self.log.info("[3/%d] 初始化 COM Worker (STA 线程)...", 4 if stdio_mode else 5)
            self.com_worker = ComWorker(config=self.config.word)
        else:
            self.log.info("[3/%d] 初始化 Add-in Worker (WS 驱动)...", 4 if stdio_mode else 5)
            self.com_worker = AddinWorker(config=self.config.word)
        if not self.com_worker.start():
            self.log.error("  ✗ Word Worker 启动失败")
            return False
        self.log.info("  → Word Worker 已就绪（%s）",
                      "COM" if sys.platform == "win32" else "Add-in WS")

        # 4. Bridge Service
        self.log.info("[4/%d] 初始化 Bridge Service...", 4 if stdio_mode else 5)
        self.service = WordBridgeService(
            com_worker=self.com_worker,
            registry=self.registry,
            ooxml_engine=self.ooxml_engine,
        )
        self.service._init_handlers()

        if stdio_mode:
            self.log.info("═══════════════════════════════════════")
            self.log.info("  所有模块初始化完成 (Stdio 模式)")
            self.log.info("═══════════════════════════════════════")
            return True

        # 5. WebSocket/HTTP Server (server mode only)
        self.log.info("[5/5] 初始化 Bridge Server...")
        self.server = BridgeServer(
            service=self.service,
            config=self.config.server,
        )
        self.log.info("  → 调试 WebSocket: ws://%s:%d",
                       self.config.server.host, self.config.server.ws_port)
        self.log.info("  → MCP HTTP:      http://%s:%d/mcp",
                       self.config.server.host, self.config.server.http_port)
        self.log.info("  → 健康检查:      http://%s:%d/health",
                       self.config.server.host, self.config.server.http_port)

        self.log.info("═══════════════════════════════════════")
        self.log.info("  所有模块初始化完成")
        self.log.info("═══════════════════════════════════════")

        return True

    # ── 运行 ──

    async def run(self) -> None:
        """
        主运行循环。

        启动 Bridge Server 并注册热键。
        等待 shutdown 信号。
        """
        if self.server is None:
            self.log.critical("请先调用 init()")
            return

        # 启动服务器
        await self.server.start()

        # 热键已废弃 — 改用 MCP word_get_active_selection 主动强挖
        self.log.info("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
        self.log.info("  WordBridge 已就绪，等待南医小助连接")
        self.log.info("  使用 word_get_active_selection 主动拉取 Word 选区")
        self.log.info("  按 Ctrl+C 停止服务")
        self.log.info("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

        # 定期清理过期 Region
        cleanup_task = asyncio.create_task(self._periodic_cleanup())

        try:
            # 等待关闭信号
            await self._shutdown_event.wait()
        except asyncio.CancelledError:
            pass
        finally:
            cleanup_task.cancel()
            try:
                await cleanup_task
            except asyncio.CancelledError:
                pass

        # 优雅关闭
        await self.shutdown()

    def shutdown_signal(self) -> None:
        """触发关闭。可以从信号处理器或热键回调中调用。"""
        self.log.info("收到关闭信号...")
        self._shutdown_event.set()

    async def shutdown(self) -> None:
        """优雅关闭所有模块。"""
        self.log.info("正在关闭 WordBridge...")

        # 1. 注销热键
        if self.hotkey_hook:
            try:
                self.hotkey_hook.unregister()
            except Exception as e:
                self.log.warning("注销热键失败: %s", e)

        # 2. 停止服务器
        if self.server:
            await self.server.stop()

        from .desensitize import close_session
        await close_session()

        # 3. 停止 COM Worker
        if self.com_worker:
            self.com_worker.stop()

        # 4. 清理过期 Region
        if self.registry:
            self.registry.cleanup_expired()

        self.log.info("WordBridge 已关闭")

    # ── 热键 ──

    def _register_hotkey(self) -> None:
        """注册全局热键。"""
        hk_config = self.config.hotkey
        modifiers = hk_config.modifiers
        key = hk_config.key

        self.hotkey_hook = create_hotkey_hook(prefer="win32")
        self.hotkey_hook.register(modifiers, key, self._on_hotkey)
        self.log.info("热键: %s+%s", "+".join(modifiers), key)

    def _on_hotkey(self) -> None:
        """
        热键触发时的回调。

        注意：此方法在热键线程中调用，不在 asyncio 事件循环中！
        所以这里不能直接 await，需要通过 submit 向 COM Worker 提交命令，
        然后等 COM Worker 完成后再通过 asyncio 事件循环推送结果。
        """
        self.log.debug("热键触发 → 捕获选区")

        if self.com_worker is None or self.registry is None:
            self.log.error("模块未初始化，无法捕获")
            return

        def capture_and_register():
            """在 COM Worker 线程中执行。"""
            future = self.com_worker.submit(CmdType.CAPTURE)
            try:
                result = future.result(timeout=10.0)
            except Exception as e:
                self.log.error("捕获超时: %s", e)
                return

            if result.success and result.data:
                captured_list = result.data
                for captured in captured_list:
                    self.registry.create(captured)
                self.log.info(
                    "✓ 已捕获 %d 个区域（可通过南医小助处理）",
                    len(captured_list),
                )
            else:
                self.log.warning(
                    "捕获失败或无选区: %s",
                    result.error or "请在 Word 中选中文字后重试"
                )

        # 在热键线程中直接调用（不再创建额外线程）
        try:
            capture_and_register()
        except Exception as e:
            self.log.error("捕获过程异常: %s", e)

    # ── 定期清理 ──

    async def _periodic_cleanup(self) -> None:
        """定期清理过期 Region。"""
        while not self._shutdown_event.is_set():
            try:
                await asyncio.wait_for(
                    self._shutdown_event.wait(), timeout=60.0
                )
            except asyncio.TimeoutError:
                if self.registry:
                    self.registry.cleanup_expired()


# ══════════════════════════════════════════════
# 入口点
# ══════════════════════════════════════════════

def main():
    """命令行入口。"""
    import argparse

    parser = argparse.ArgumentParser(
        description="WordBridge — 南医小助 ↔ Word 后台桥接服务",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
示例:
  wordbridge                  # 使用默认配置启动
  wordbridge -c config.yaml   # 指定配置文件
  wordbridge --no-hotkey      # 不注册热键
        """,
    )
    parser.add_argument(
        "-c", "--config",
        type=str,
        default=None,
        help="配置文件路径 (默认: 当前目录下的 config.yaml)",
    )
    parser.add_argument(
        "--no-hotkey",
        action="store_true",
        help="禁用全局热键（仅通过南医小助 MCP 工具调用）",
    )
    parser.add_argument(
        "--debug",
        action="store_true",
        help="启用 DEBUG 日志级别",
    )
    parser.add_argument(
        "--stdio",
        action="store_true",
        help="Stdio 模式：从 stdin 读取 JSON-RPC，向 stdout 写入响应（供 MCP 托管使用）",
    )
    args = parser.parse_args()

    # 创建 App
    app = WordBridgeApp(config_path=args.config, stdio_mode=args.stdio)

    # Debug 模式
    if args.debug:
        app.config.bridge.log_level = "DEBUG"
        app.log.setLevel(10)  # DEBUG

    # 初始化
    if not app.init(stdio_mode=args.stdio):
        sys.exit(1)

    # 信号处理
    def _signal_handler():
        app.shutdown_signal()

    try:
        # Windows 上使用 signal
        signal.signal(signal.SIGINT, lambda s, f: _signal_handler())
        signal.signal(signal.SIGTERM, lambda s, f: _signal_handler())
    except (ValueError, OSError):
        # 在非主线程中 signal 不可用
        pass

    # 运行
    if args.stdio:
        asyncio.run(_run_stdio(app))
    else:
        try:
            asyncio.run(app.run())
        except KeyboardInterrupt:
            app.log.info("收到 KeyboardInterrupt")


async def _run_stdio(app: WordBridgeApp) -> None:
    """Run the FastMCP stdio transport for a MindLink-managed process."""
    from .mcp_server import create_mcp_server

    mcp = create_mcp_server(app.service, app.config.bridge.version)
    app.log.info("FastMCP Stdio 服务已启动")
    cleanup_task = asyncio.create_task(app._periodic_cleanup())
    try:
        await mcp.run_stdio_async(show_banner=False)
    finally:
        cleanup_task.cancel()
        await asyncio.gather(cleanup_task, return_exceptions=True)
        await app.shutdown()


if __name__ == "__main__":
    main()
