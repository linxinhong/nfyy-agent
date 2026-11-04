"""
Word 加载项服务端（macOS）：HTTPS 静态托管 + WS 命令通道。

macOS 没有 COM，Word 的读写只能由加载项的 JS 完成（且仅面板打开时有运行时）。
本模块提供：
  1. AddinHub —— AddinWorker（工作线程）与 asyncio WS 之间线程安全的命令往返通道
  2. build_addin_app —— starlette ASGI：托管 taskpane.html/js + /addin-ws WebSocket

BridgeServer 在非 win32 平台用 uvicorn + 自签证书把该 app 起在 9879（HTTPS）。
taskpane 页面与 API 同源，从而免掉 CORS 与 mixed-content 问题。
"""
from __future__ import annotations

import asyncio
import json
import os
import threading
import uuid
from concurrent.futures import Future
from pathlib import Path
from typing import Any, Optional

from starlette.applications import Starlette
from starlette.responses import RedirectResponse
from starlette.routing import Mount, Route, WebSocketRoute
from starlette.staticfiles import StaticFiles
from starlette.websockets import WebSocket

from .utils import get_logger

logger = get_logger("addin_server")

# 包根目录（wordbridge/），addin 静态文件与证书都相对它定位
_PKG_DIR = Path(__file__).resolve().parent.parent

ADDIN_HOST = "127.0.0.1"
ADDIN_PORT = 9879  # 与侧载 manifest 的 SourceLocation 绑定，勿改
ADDIN_DIR = _PKG_DIR / "addin"
# 证书按机器生成并保存在宿主的可写目录（WORDBRIDGE_CERT_DIR），私钥从不随安装包分发。
_CERT_DIR = Path(os.environ.get("WORDBRIDGE_CERT_DIR") or (_PKG_DIR / "certs"))
CERT_FILE = _CERT_DIR / "localhost.pem"
KEY_FILE = _CERT_DIR / "localhost-key.pem"

# 只接受由本服务托管的任务窗格页面发起的 WebSocket，防止任意网页冒充加载项。
ALLOWED_WS_ORIGINS = frozenset({
    f"https://localhost:{ADDIN_PORT}",
    f"https://127.0.0.1:{ADDIN_PORT}",
})

# 单条命令等待加载项响应的默认超时（秒）
REQUEST_TIMEOUT = 30.0


class AddinHub:
    """工作线程 ↔ asyncio WS 的线程安全命令通道。

    - AddinWorker._dispatch（工作线程）调用 :meth:`send_request`，注册一个 Future，
      把命令推给已连接的加载项，再阻塞等待响应。
    - ASGI 的 WS handler（asyncio 事件循环）把客户端回包交给 :meth:`handle_message`，
      据此结算对应的 Future。

    这样 WorkerBase 的 submit/Future/重试语义对 AddinWorker 完全透明，
    上层 WordBridgeService / mcp_server 无需感知驱动差异。
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._pending: dict[str, Future] = {}
        self._clients: list[WebSocket] = []
        self._loop: Optional[asyncio.AbstractEventLoop] = None

    # ── 生命周期（asyncio 侧） ──

    def bind_loop(self, loop: asyncio.AbstractEventLoop) -> None:
        self._loop = loop

    def register(self, ws: WebSocket) -> None:
        with self._lock:
            if ws not in self._clients:
                self._clients.append(ws)
        logger.info("Word 加载项已接入（当前 %d 个面板）", len(self._clients))

    def unregister(self, ws: WebSocket) -> None:
        with self._lock:
            if ws in self._clients:
                self._clients.remove(ws)
        self._fail_all("加载项连接已断开")
        logger.info("Word 加载项已断开（剩余 %d 个面板）", len(self._clients))

    def handle_message(self, data: Any) -> None:
        """由 asyncio WS handler 调用：结算对应 request_id 的 Future。"""
        if not isinstance(data, dict):
            return
        req_id = data.get("id")
        if not req_id:
            return
        with self._lock:
            fut = self._pending.pop(req_id, None)
        if fut is None or fut.done():
            return
        if data.get("ok"):
            fut.set_result(data.get("result"))
        else:
            fut.set_exception(RuntimeError(data.get("error") or "加载项执行失败"))

    # ── 工作线程侧 ──

    @property
    def connected(self) -> bool:
        with self._lock:
            return bool(self._clients)

    def _active_client(self) -> Optional[WebSocket]:
        with self._lock:
            return self._clients[-1] if self._clients else None

    def send_request(self, method: str, params: dict, timeout: float = REQUEST_TIMEOUT) -> Any:
        """把命令推给加载项并阻塞等待响应（在工作线程内调用）。"""
        client = self._active_client()
        loop = self._loop
        if client is None or loop is None:
            raise RuntimeError("没有已连接的 Word 加载项（面板未打开或未接入桥）")

        req_id = uuid.uuid4().hex[:8]
        fut: Future = Future()
        with self._lock:
            self._pending[req_id] = fut

        message = json.dumps(
            {"id": req_id, "method": method, "params": params or {}},
            ensure_ascii=False,
        )
        try:
            send_fut = asyncio.run_coroutine_threadsafe(client.send_text(message), loop)
            send_fut.result(timeout=3.0)
        except Exception:
            with self._lock:
                self._pending.pop(req_id, None)
            raise RuntimeError("向加载项发送命令失败")

        try:
            return fut.result(timeout=timeout)
        except Exception:
            with self._lock:
                self._pending.pop(req_id, None)
            raise
        finally:
            with self._lock:
                self._pending.pop(req_id, None)

    def _fail_all(self, error: str) -> None:
        with self._lock:
            pending = list(self._pending.values())
            self._pending.clear()
        for fut in pending:
            if not fut.done():
                fut.set_exception(RuntimeError(error))


# ── ASGI app ──


def build_addin_app(addin_dir: Path, hub: AddinHub) -> Starlette:
    """构建托管 taskpane 静态文件 + /addin-ws WebSocket 的 ASGI app。"""

    async def ws_endpoint(websocket: WebSocket) -> None:
        origin = websocket.headers.get("origin")
        if origin is not None and origin not in ALLOWED_WS_ORIGINS:
            logger.warning("拒绝来源不明的加载项连接: %s", origin)
            await websocket.close(code=1008)
            return
        await websocket.accept()
        hub.register(websocket)
        try:
            async for raw in websocket.iter_text():
                try:
                    data = json.loads(raw)
                except (json.JSONDecodeError, TypeError):
                    continue
                hub.handle_message(data)
        except Exception:
            pass
        finally:
            hub.unregister(websocket)

    async def index(_request):
        return RedirectResponse("/taskpane.html")

    if not addin_dir.exists():
        addin_dir.mkdir(parents=True, exist_ok=True)

    routes = [
        WebSocketRoute("/addin-ws", ws_endpoint),
        Route("/", index),
        Mount("/", app=StaticFiles(directory=str(addin_dir), html=True), name="addin_static"),
    ]
    return Starlette(routes=routes)
