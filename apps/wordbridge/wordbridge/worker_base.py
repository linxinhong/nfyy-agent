"""Worker 基类:线程 + 命令队列 + Future 调度壳(平台无关)。

从 com_worker.ComWorker 抽取出来的可复用部分。COM Worker 与 Add-in Worker
各自继承本类并重写 `_worker_loop` / `_dispatch`,共享同一套 submit→Future、
命令队列、重试调度与便捷方法的语义,保证上层 WordBridgeService / mcp_server
对"用哪种 Worker 驱动 Word"完全无感知。
"""
from __future__ import annotations

import queue
import threading
import time
import uuid
from concurrent.futures import Future
from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Optional

from .utils import get_logger

logger = get_logger("worker_base")


class CmdType(Enum):
    """命令类型枚举。"""
    CAPTURE = "capture"                 # 捕获选区（支持多区域）
    FIND_TEXT = "find_text"             # 在文档中查找文本并捕获为 region（仅定位不修改）
    READ_DOC = "read_doc"               # 读取整个文档正文并捕获为 region（仅读取）
    OPEN_DOC = "open_doc"               # 打开指定路径的文档
    APPLY = "apply"                     # 应用 OOXML 修改
    FORCE_APPLY = "force_apply"         # 强制应用（忽略冲突）
    APPLY_TEXT = "apply_text"           # 直接文本替换（不经过 OOXML）
    APPLY_PATCH = "apply_patch"         # 精准 Find/Replace（修订模式保留差异）
    FIX_SIGNATURE = "fix_signature"     # 签名页分节符+签署页页脚
    DIFF_CHECK = "diff_check"           # 冲突检测
    HEALTH_CHECK = "health_check"       # Word 连接健康检查
    GET_DOC_INFO = "get_doc_info"       # 获取文档信息
    LIST_DOCS = "list_docs"             # 列出打开的所有文档
    SHUTDOWN = "shutdown"               # 关闭 Worker


@dataclass
class ComCommand:
    """发送到 Worker 的命令。"""
    cmd_id: str = field(default_factory=lambda: uuid.uuid4().hex[:8])
    cmd_type: CmdType = CmdType.HEALTH_CHECK
    payload: dict = field(default_factory=dict)
    future: Optional[Future] = None     # 跨线程返回结果的 Future


@dataclass
class ComResult:
    """Worker 返回的结果。"""
    cmd_id: str
    success: bool
    data: Any = None
    error: Optional[str] = None
    error_code: Optional[int] = None
    took_ms: float = 0.0


class WorkerBase:
    """单线程任务队列 Worker 调度壳。

    设计要点:
      - 独立线程 + 任务队列（线程安全）序列化所有命令
      - 每个命令通过 concurrent.futures.Future 返回结果
      - 命令失败自动重试（重试策略由子类 `_should_retry` 决定）

    子类需覆盖:
      - `_worker_loop()`  —— 线程主循环（COM: STA 初始化 + 消息泵;Add-in: WS 循环）
      - `_dispatch(cmd)`  —— 实际执行命令并返回结果
    可选覆盖:
      - `_should_retry(e, attempt, cmd)` —— 失败重试策略（默认不重试）
    """

    #: 重试参数默认值（从 config 的 com_* 字段读取,命名沿用原 COM 配置）
    retry_max = 5
    retry_delay_ms = 200
    retry_backoff = 2.0

    def __init__(self, config: Any = None):
        self.config = config
        self._queue: queue.Queue = queue.Queue(maxsize=128)
        self._thread: Optional[threading.Thread] = None
        self._running = False
        self._shutdown_event = threading.Event()
        if config is not None:
            self.retry_max = int(getattr(config, "com_retry_max", self.retry_max))
            self.retry_delay_ms = int(getattr(config, "com_retry_delay_ms", self.retry_delay_ms))
            self.retry_backoff = float(getattr(config, "com_retry_backoff", self.retry_backoff))

    # ── 子类覆盖点 ──

    @property
    def thread_name(self) -> str:
        return "WordBridge-Worker"

    @property
    def health_check_timeout(self) -> float:
        return 10.0

    def _worker_loop(self) -> None:
        raise NotImplementedError

    def _dispatch(self, cmd: ComCommand) -> Any:
        raise NotImplementedError

    def _should_retry(self, e: Exception, attempt: int, cmd: ComCommand) -> bool:
        return False

    def _pump(self) -> None:
        """子类可选覆盖（如 COM 消息泵）。"""

    # ── 启动 / 停止 ──

    def start(self) -> bool:
        """启动 Worker 线程并同步等待就绪（通过 HEALTH_CHECK 握手）。"""
        if self._running:
            logger.warning("%s 已在运行中", self.thread_name)
            return True

        self._running = True
        self._thread = threading.Thread(
            target=self._worker_loop,
            name=self.thread_name,
            daemon=True,
        )
        self._thread.start()
        logger.info("%s 线程已启动", self.thread_name)

        future: Future = Future()
        self._queue.put(ComCommand(cmd_type=CmdType.HEALTH_CHECK, future=future))
        try:
            result = future.result(timeout=self.health_check_timeout)
            if result.success:
                logger.info("%s 已就绪", self.thread_name)
                return True
            logger.error("%s 连接失败: %s", self.thread_name, result.error)
            return False
        except Exception as e:
            logger.error("等待 %s 就绪超时: %s", self.thread_name, e)
            return False

    def stop(self, timeout: float = 5.0) -> None:
        """优雅关闭 Worker。"""
        if not self._running:
            return

        logger.info("正在关闭 %s ...", self.thread_name)
        self._running = False
        self._shutdown_event.set()

        try:
            self._queue.put(ComCommand(cmd_type=CmdType.SHUTDOWN), timeout=1.0)
        except queue.Full:
            pass

        if self._thread and self._thread.is_alive():
            self._thread.join(timeout=timeout)

        logger.info("%s 已关闭", self.thread_name)

    # ── 公共 API（从任何线程调用）──

    def submit(self, cmd_type: CmdType, payload: dict = None) -> Future:
        """提交命令,线程安全,返回 Future。"""
        if not self._running:
            future: Future = Future()
            future.set_result(ComResult(
                cmd_id="dead",
                success=False,
                error="Worker 未运行",
            ))
            return future

        cmd = ComCommand(
            cmd_type=cmd_type,
            payload=payload or {},
            future=Future(),
        )
        try:
            self._queue.put_nowait(cmd)
        except queue.Full:
            cmd.future.set_result(ComResult(cmd_id=cmd.cmd_id, success=False, error="命令队列已满，请稍后重试"))
        return cmd.future

    # ── 便捷方法（与 ComWorker 原接口一一对应）──

    def capture(self) -> Future:
        """捕获当前选区。"""
        return self.submit(CmdType.CAPTURE)

    def find_text(self, text: str, max_matches: int = 20) -> Future:
        """在文档中查找文本并捕获为 region（仅定位不修改）。"""
        return self.submit(CmdType.FIND_TEXT, {"text": text, "max_matches": max_matches})

    def read_document(self, doc_path: str = "") -> Future:
        """读取整个文档正文并捕获为 region（仅读取）。"""
        return self.submit(CmdType.READ_DOC, {"doc_path": doc_path})

    def list_documents(self) -> Future:
        """列出当前 Word 实例中打开的文档。"""
        return self.submit(CmdType.LIST_DOCS)

    def open_document(self, path: str) -> Future:
        """打开指定路径的文档。"""
        return self.submit(CmdType.OPEN_DOC, {"path": path})

    def apply(self, region_id: str, new_ooxml: str, force: bool = False, doc_path: str = "", expected_hash: str = "") -> Future:
        """应用 OOXML 修改。"""
        return self.submit(
            CmdType.FORCE_APPLY if force else CmdType.APPLY,
            {"region_id": region_id, "new_ooxml": new_ooxml, "doc_path": doc_path, "expected_hash": expected_hash},
        )

    def apply_text(self, region_id: str, new_text: str, doc_path: str = "") -> Future:
        """直接文本替换（不经过 OOXML）。"""
        return self.submit(
            CmdType.APPLY_TEXT,
            {"region_id": region_id, "new_text": new_text, "doc_path": doc_path},
        )

    def apply_patch(self, region_id: str, patches: list, doc_path: str = "", expected_hash: str = "") -> Future:
        """精准 Find/Replace 替换。"""
        return self.submit(
            CmdType.APPLY_PATCH,
            {"region_id": region_id, "patches": patches, "doc_path": doc_path, "expected_hash": expected_hash},
        )

    def diff_check(self, region_id: str, doc_path: str = "") -> Future:
        """冲突检测。"""
        return self.submit(CmdType.DIFF_CHECK, {"region_id": region_id, "doc_path": doc_path})

    # ── 队列调度 ──

    def _drain_command_queue(self) -> None:
        """处理队列中所有待处理命令（非阻塞）。"""
        for _ in range(16):
            try:
                cmd = self._queue.get_nowait()
            except queue.Empty:
                break

            try:
                self._execute_command(cmd)
            except Exception as e:
                logger.error("执行命令 %s 时发生未捕获异常: %s", cmd.cmd_type, e)
                if cmd.future and not cmd.future.done():
                    cmd.future.set_result(ComResult(
                        cmd_id=cmd.cmd_id,
                        success=False,
                        error=str(e),
                    ))

    def _execute_command(self, cmd: ComCommand) -> None:
        """执行单个命令,带自动重试。"""
        if cmd.future and not cmd.future.set_running_or_notify_cancel():
            return
        if cmd.cmd_type == CmdType.SHUTDOWN:
            logger.info("收到 SHUTDOWN 命令")
            if cmd.future and not cmd.future.done():
                cmd.future.set_result(ComResult(
                    cmd_id=cmd.cmd_id,
                    success=True,
                    data="shutting_down",
                ))
            return

        start_time = time.time()
        last_error = None
        last_error_code = None

        for attempt in range(self.retry_max + 1):
            try:
                data = self._dispatch(cmd)
                took_ms = (time.time() - start_time) * 1000
                if cmd.future and not cmd.future.done():
                    cmd.future.set_result(ComResult(
                        cmd_id=cmd.cmd_id,
                        success=True,
                        data=data,
                        took_ms=took_ms,
                    ))
                return

            except Exception as e:
                last_error = str(e)
                last_error_code = getattr(e, "hresult", None)
                if not self._should_retry(e, attempt, cmd):
                    break

        took_ms = (time.time() - start_time) * 1000
        logger.error(
            "命令 %s (%s) 失败: %s，已重试 %d 次",
            cmd.cmd_type, cmd.cmd_id, last_error, self.retry_max,
        )
        if cmd.future and not cmd.future.done():
            cmd.future.set_result(ComResult(
                cmd_id=cmd.cmd_id,
                success=False,
                error=last_error,
                error_code=last_error_code,
                took_ms=took_ms,
            ))

    def _fail_pending(self, error: str) -> None:
        """线程退出时结算队列中所有挂起命令。"""
        while True:
            try:
                pending = self._queue.get_nowait()
            except queue.Empty:
                break
            if pending.future and not pending.future.done():
                pending.future.set_result(ComResult(
                    cmd_id=pending.cmd_id,
                    success=False,
                    error=error,
                ))
