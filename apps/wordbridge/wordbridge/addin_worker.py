"""Word 加载项 Worker（macOS）:经 WebSocket 驱动 taskpane.js,替代 COM。

Windows 上 Word 由 COM（pywin32）直连;macOS 没有 COM,唯一能读写 Word 的是
加载项的 JS（且仅面板打开时有运行时）。本 Worker 通过 :class:`AddinHub` 把命令
推给加载项,加载项在 ``Word.run`` 里执行后按 request_id 回传结果,由 submit 返回
的 Future 结算。

上层语义与 ComWorker 完全一致:WorkerBase 提供 submit / Future / 队列 / 重试壳,
本类只负责把 ``CmdType`` 翻译成加载项方法名 + 参数,并借 AddinHub 完成往返。
"""
from __future__ import annotations

import base64
import time
from pathlib import Path
from typing import Any, Optional

from .addin_server import AddinHub
from .config import get_config
from .worker_base import CmdType, ComCommand, WorkerBase
from .utils import get_logger

logger = get_logger("addin_worker")


class AddinWorker(WorkerBase):
    """WS 驱动的 Word 加载项 Worker（macOS）。"""

    @property
    def thread_name(self) -> str:
        return "WordBridge-Addin"

    @property
    def health_check_timeout(self) -> float:
        return 2.0

    def __init__(self, config: Optional[Any] = None):
        self.addin_hub = AddinHub()
        super().__init__(config or get_config().word)

    # ── 线程主循环 ──

    def _worker_loop(self) -> None:
        # 本线程只承载命令队列调度;真正的 WS 收发在 BridgeServer 起的 asyncio
        # HTTPS 服务器里完成,通过 self.addin_hub 与本线程桥接。
        while self._running:
            self._drain_command_queue()
            time.sleep(0.02)
        self._fail_pending("Addin Worker 已停止")

    # ── 命令翻译 ──

    def _dispatch(self, cmd: ComCommand) -> Any:
        if cmd.cmd_type == CmdType.HEALTH_CHECK:
            return {
                "status": "ok",
                "mode": "addin",
                "connected": self.addin_hub.connected,
            }

        method, params = self._map_command(cmd)
        return self.addin_hub.send_request(method, params, timeout=self._command_timeout(cmd))

    @staticmethod
    def _command_timeout(cmd: ComCommand) -> float:
        # 写操作可能涉及 Word 重算,给更宽裕的超时;读操作取默认。
        if cmd.cmd_type in (CmdType.APPLY, CmdType.FORCE_APPLY, CmdType.APPLY_TEXT, CmdType.APPLY_PATCH):
            return 30.0
        return 20.0

    def _map_command(self, cmd: ComCommand) -> tuple[str, dict]:
        p = cmd.payload or {}
        t = cmd.cmd_type

        if t == CmdType.CAPTURE:
            return "capture", {}

        if t == CmdType.FIND_TEXT:
            return "find_text", {"text": p["text"], "max_matches": p.get("max_matches", 20)}

        if t == CmdType.READ_DOC:
            return "read_document", {"doc_path": p.get("doc_path", "")}

        if t == CmdType.APPLY:
            return "apply_ooxml", {
                "region_id": p["region_id"],
                "new_ooxml": p["new_ooxml"],
                "doc_path": p.get("doc_path", ""),
                "expected_hash": p.get("expected_hash", ""),
            }

        if t == CmdType.FORCE_APPLY:
            return "apply_ooxml", {
                "region_id": p["region_id"],
                "new_ooxml": p["new_ooxml"],
                "doc_path": p.get("doc_path", ""),
            }

        if t == CmdType.APPLY_TEXT:
            return "apply_text", {
                "region_id": p["region_id"],
                "new_text": p["new_text"],
                "doc_path": p.get("doc_path", ""),
            }

        if t == CmdType.APPLY_PATCH:
            return "apply_patch", {
                "region_id": p["region_id"],
                "patches": p["patches"],
                "doc_path": p.get("doc_path", ""),
                "expected_hash": p.get("expected_hash", ""),
            }

        if t == CmdType.DIFF_CHECK:
            return "diff_check", {
                "region_id": p["region_id"],
                "doc_path": p.get("doc_path", ""),
            }

        if t == CmdType.GET_DOC_INFO:
            return "get_doc_info", {}

        if t == CmdType.LIST_DOCS:
            return "list_documents", {}

        if t == CmdType.OPEN_DOC:
            # macOS 加载项无法直接打开任意文件路径（沙箱）；改为读文件字节 →
            # Word.createDocument(base64) 新建并打开。当前仅 word_create_document 走此路径。
            path = p.get("path", "")
            data = Path(path).read_bytes()
            return "create_document", {
                "name": Path(path).name,
                "path": path,
                "base64": base64.b64encode(data).decode("ascii"),
            }

        raise ValueError(f"未知命令类型: {t}")
