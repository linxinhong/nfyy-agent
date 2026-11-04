"""
COM STA 线程管理模块
────────────────────────────────────────

设计模式：单线程任务队列（Worker Thread + Queue）
  ┌──────────────┐     queue.put(Cmd)    ┌──────────────────┐
  │  调用方线程   │ ───────────────────▶  │  COM STA Worker  │
  │  (asyncio)   │ ◀───────────────────  │  (pythoncom)     │
  │              │    future.set_result  │                  │
  └──────────────┘                       └──────┬───────────┘
                                                │ Dispatch
                                                ▼
                                          Microsoft Word

核心约束：
  1. 所有 COM 调用必须在 STA Worker 线程中执行
  2. Worker 线程使用 pythoncom.CoInitialize() 初始化 COM
  3. 消息泵 pythoncom.PumpWaitingMessages() 保持 COM 活性
  4. 返回结果通过 concurrent.futures.Future 跨线程传递
  5. 严禁从其他线程直接调用 self.word_app 等 COM 对象
"""

from __future__ import annotations

import queue
import threading
import time
import uuid
from concurrent.futures import Future
from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Callable, Optional

try:
    import pythoncom  # type: ignore
    import win32com.client  # type: ignore
    from pywintypes import com_error  # type: ignore
except ImportError:  # macOS 无 pywin32:仅保证模块可导入,ComWorker 不会被实例化
    pythoncom = None  # type: ignore
    win32com = None  # type: ignore
    com_error = Exception  # type: ignore

from .config import WordConfig, get_config
from .utils import get_logger, text_hash, truncate
from .selection_cache import SelectionCache, SelectionSnapshot

logger = get_logger("com_worker")


# ══════════════════════════════════════════════
# 命令系统
# ══════════════════════════════════════════════

from .worker_base import CmdType, ComCommand, ComResult, WorkerBase  # noqa: F401


# ══════════════════════════════════════════════
# Word 封装对象（仅在 STA 线程内实例化）
# ══════════════════════════════════════════════

class WordHandler:
    """
    Microsoft Word COM 对象的线程安全封装。

    重要：此类的所有实例方法必须在 STA 线程内调用。
    外部不应直接持有此对象的引用。
    """

    def __init__(self, prog_id: str = "Word.Application", config: Optional[WordConfig] = None):
        self.prog_id = prog_id
        self._word: Optional[Any] = None  # win32com Dispatch object
        self._connected: bool = False
        self.config = config or WordConfig()
        self.selection_cache = SelectionCache(
            self.config.selection_debounce_seconds,
            self.config.selection_cache_ttl_seconds,
            self.config.selection_cache_max_chars,
        )

    # ── 连接管理 ──

    def connect(self) -> bool:
        """
        连接到正在运行的 Word 实例（不自动启动新的，避免打开 Hermes 就弹 Word）。
        在 STA 线程内调用。
        """
        try:
            # 只连已有的 Word 实例，不新建（新建会弹 Word 窗口）
            self._word = win32com.client.GetActiveObject(self.prog_id)
            self._connected = True
            logger.info("已连接到运行中的 Word 实例")
            return True
        except com_error:
            self._connected = False
            self._word = None
            logger.debug("Word 未运行，将按需连接")
            return False

    def disconnect(self) -> None:
        """断开连接（不关闭 Word）。"""
        self._connected = False
        # 不调用 Quit，让用户继续使用 Word
        self._word = None
        self.selection_cache.clear()

    @property
    def is_connected(self) -> bool:
        return self._connected and self._word is not None

    # ── 基础属性 ──

    def _check_connection(self) -> None:
        if not self.is_connected:
            # 按需连接（Word 可能刚被用户打开）
            self.connect()
        if not self.is_connected:
            raise RuntimeError("未检测到 Word，请先打开 Microsoft Word")

    def get_active_document(self):
        """获取当前活动文档。"""
        self._check_connection()
        try:
            return self._word.ActiveDocument
        except com_error as e:
            if e.hresult == -2147418111:  # RPC_E_CALL_REJECTED
                raise
            return None

    def _find_doc_by_path(self, doc_path: str):
        """按路径找到文档（支持多文档：不依赖"当前激活文档"）。

        指定源文档不存在时返回 None，不能误用活动文档。
        """
        if doc_path:
            try:
                docs = self._word.Documents
                for i in range(1, docs.Count + 1):
                    try:
                        d = docs(i)
                        if str(d.FullName) == doc_path:
                            return d
                    except com_error:
                        continue
            except com_error:
                pass
            return None
        return self.get_active_document()

    def get_doc_info(self) -> dict:
        """获取活动文档的基本信息（Word 未运行时返回 no_word，不报错）。"""
        if not self.is_connected:
            self.connect()
        if not self.is_connected:
            return {"status": "no_word"}
        try:
            doc = self.get_active_document()
            if doc is None:
                return {"status": "no_document"}
            return {
                "status": "ok",
                "name": doc.Name,
                "path": doc.FullName,
                "saved": doc.Saved,
                "paragraphs": doc.Paragraphs.Count,
            }
        except com_error:
            return {"status": "error"}

    def list_documents(self) -> list[dict]:
        """列出所有打开的文档（名称、路径及活动状态）。"""
        self._check_connection()
        try:
            docs = self._word.Documents
            active_path = ""
            try:
                active_path = str(self._word.ActiveDocument.FullName)
            except com_error:
                pass
            result = []
            for index in range(1, docs.Count + 1):
                try:
                    doc = docs(index)
                    path = str(doc.FullName)
                    result.append({
                        "name": str(doc.Name),
                        "path": path,
                        "active": path == active_path,
                    })
                except com_error:
                    continue
            return result
        except com_error as exc:
            logger.error("列出文档时 COM 错误: %s", exc)
            return []

    def open_document(self, path: str) -> dict:
        """打开指定路径的文档并激活（用于生成后自动在 Word 中打开）。"""
        self._check_connection()
        if not path:
            return {"status": "error", "message": "缺少文档路径"}
        try:
            doc = self._word.Documents.Open(path)
            doc.Activate()
            return {
                "status": "ok",
                "name": str(doc.Name),
                "path": str(doc.FullName),
            }
        except com_error as e:
            logger.error("打开文档时 COM 错误: %s (HRESULT: %s)", e, hex(e.hresult))
            return {"status": "error", "message": str(e)}

    # ── 选区捕获 ──

    def observe_selection(self) -> None:
        """Read-only polling: no bookmarks, OOXML serialization, DB or network IO."""
        if not self.config.selection_cache_enabled:
            return
        try:
            if not self.is_connected and not self.connect():
                self.selection_cache.clear()
                return
            sel = self._word.Selection
            now = time.monotonic()
            if sel.Type <= 1:
                self.selection_cache.observe(None, now)
                return
            rng = sel.Range.Duplicate
            if rng.End - rng.Start > self.config.selection_cache_max_chars:
                self.selection_cache.clear()
                return
            text = str(rng.Text)
            snapshot = SelectionSnapshot(
                str(rng.Document.FullName), int(rng.StoryType),
                int(rng.Start), int(rng.End), text, text_hash(text), rng,
            ) if text.strip() else None
            self.selection_cache.observe(snapshot, now)
        except com_error as e:
            # A busy Word is transient; a disconnected COM reference is not.
            if e.hresult not in (-2147418111, -2147417846):
                self.disconnect()
        except Exception:
            self.selection_cache.clear()
            logger.debug("选区观察暂不可用", exc_info=True)

    def _capture_cached_selection(self) -> list[dict]:
        snapshot = self.selection_cache.get(time.monotonic())
        if snapshot is None:
            return []
        rng = snapshot.range_ref
        try:
            doc = self._find_doc_by_path(snapshot.doc_path)
            if (doc is None or str(rng.Document.FullName) != snapshot.doc_path
                    or text_hash(str(rng.Text)) != snapshot.text_hash):
                raise RuntimeError("缓存选区已变化或源文档已关闭，请重新选择")
            return self._capture_range(rng, doc, source="cache")
        finally:
            # A cached selection is a one-shot fallback, never a stale write anchor.
            self.selection_cache.clear()

    def _capture_range(self, rng, doc, source="active") -> list[dict]:
        text = str(rng.Text)
        if not text.strip():
            return []
        ooxml = str(rng.XML)
        region_id = f"hb_{uuid.uuid4().hex[:16]}"
        doc.Bookmarks.Add(region_id, rng)
        logger.info("捕获选区 %s (%d chars, %s)", region_id, len(text), source)
        return [{
            "region_id": region_id, "text": text, "ooxml": ooxml,
            "hash": text_hash(text), "bookmark": region_id,
            "doc_path": str(doc.FullName), "source": source,
        }]

    def capture_selected_ranges(self) -> list[dict]:
        """
        捕获当前所有选中区域。

        设计要点：
        1. 使用 Selection.Range.Duplicate 创建独立 Range
           → 此后与用户光标完全解耦
        2. 对每个 Range 插入隐形书签用于后续定位
        3. 同时获取纯文本（发给 LLM）和 OOXML（保留格式）
        4. 整个操作 < 50ms，人眼无感

        返回：list[dict]，每个 dict 包含 region_id, text, ooxml, hash, bookmark
        """
        self._check_connection()

        try:
            doc = self.get_active_document()
            if doc is None:
                return []

            sel = self._word.Selection

            # 检查是否有实际选中的内容
            # WdSelectionType: wdNoSelection=0, wdSelectionIP=1, wdSelectionNormal=2, ...
            if sel.Type <= 1:  # 无选区 或 仅插入点光标（无文本选中）
                return self._capture_cached_selection()

            # 获取选区的纯文本长度
            raw_text = str(sel.Text)
            if not raw_text or not raw_text.strip():
                return []

            # ─── 核心操作：创建独立 Range ───
            rng = sel.Range.Duplicate   # ← 脱离 Selection

            text = str(rng.Text)
            if not text.strip():
                return []

            return self._capture_range(rng, doc)

        except com_error as e:
            logger.error("捕获选区时 COM 错误: %s (HRESULT: %s)", e, hex(e.hresult))
            return []

    def find_text(self, text: str, max_matches: int = 20) -> list[dict]:
        """在活动文档中查找 text，把每个匹配捕获成 region（仅定位，不修改）。

        用于「不用光标选定、直接告诉 agent 改哪些点」：agent 用本方法定位原文，
        拿到 region_id 后再走 word_patch 做字符级最小修订。
        """
        self._check_connection()
        results: list[dict] = []
        if not text:
            return results
        try:
            doc = self.get_active_document()
            if doc is None:
                return results
            rng = doc.Content
            count = 0
            while count < max_matches:
                find = rng.Find
                find.ClearFormatting()
                find.Text = text
                find.Forward = True
                find.Wrap = 0  # wdFindStop
                find.MatchCase = True
                if not find.Execute():
                    break
                match_rng = rng.Duplicate  # Execute 后 rng 已重定位到匹配文本
                captured = self._capture_range(match_rng, doc, source="find")
                results.extend(captured)
                count += 1
                rng.Collapse(0)  # wdCollapseEnd
                rng.End = doc.Content.End
        except com_error as e:
            logger.error("查找文本时 COM 错误: %s (HRESULT: %s)", e, hex(e.hresult))
        except Exception as e:
            logger.error("查找文本失败: %s", e)
        return results

    def read_document(self, doc_path: str = "") -> dict:
        """读取打开的 Word 文档正文，捕获为 region（仅读取，供 word_patch 最小修订）。

        只读正文文本 + 建书签；不取 OOXML（word_patch 走 Range.Find，不需要）。
        """
        self._check_connection()
        try:
            doc = self._find_doc_by_path(doc_path) if doc_path else self.get_active_document()
            if doc is None:
                return {"status": "error", "message": "找不到指定文档，或没有打开的 Word 文档"}
            rng = doc.Content
            text = str(rng.Text)
            if not text.strip():
                return {"status": "error", "message": "文档正文为空"}
            region_id = f"hb_{uuid.uuid4().hex[:16]}"
            doc.Bookmarks.Add(region_id, rng)
            logger.info("读取文档 %s (%d chars)", str(doc.Name), len(text))
            return {
                "status": "ok",
                "name": str(doc.Name),
                "path": str(doc.FullName),
                "capture": {
                    "region_id": region_id,
                    "text": text,
                    "ooxml": "",
                    "hash": text_hash(text),
                    "bookmark": region_id,
                    "doc_path": str(doc.FullName),
                    "source": "read_document",
                },
            }
        except com_error as e:
            logger.error("读取文档时 COM 错误: %s", e)
            return {"status": "error", "message": str(e)}

    # ── 文本应用 ──

    def apply_modification(
        self,
        region_id: str,
        new_text: str,
        force: bool = False,
    ) -> dict:
        """
        通过书签定位并静默替换文本。

        全程不使用 Selection 对象，光标/滚动位置完全不受影响。

        参数：
          region_id: 书签名
          new_text:  AI 处理后的新文本（纯文本）
          force:     True = 忽略冲突，强制覆盖

        返回状态：
          applied   → 修改已应用
          conflict  → 检测到冲突（非 force 模式）
          not_found → 书签/Region 不存在
          error     → 应用失败
        """
        self._check_connection()

        try:
            doc = self.get_active_document()
            if doc is None:
                return {"status": "error", "reason": "无活动文档"}

            # 1. 通过书签定位
            try:
                bookmark = doc.Bookmarks(region_id)
            except com_error:
                return {
                    "status": "not_found",
                    "reason": f"书签 {region_id} 不存在（可能被用户删除）"
                }

            target_range = bookmark.Range
            current_text = str(target_range.Text)
            current_hash = text_hash(current_text)

            # 2. 获取原始的 hash 和 OOXML（由外部传入，从 Registry 查）
            #    这里只做应用操作，hash 比对由调用方在 RegionRegistry 中处理
            #    如果是 force 模式，直接替换

            if not force:
                # 非 force 模式：这里不做冲突判断
                # 冲突判断由 RegionRegistry 在调用 apply 之前完成
                pass

            # 3. 构建新 OOXML
            #    OOXML 的构建由 OoxmlEngine 完成，这里只做插入
            #    如果调用方传入了 new_ooxml，直接用；否则这里只记录结果
            #    实际 OOXML 生成在 ws_server 层完成，这里聚焦 COM 操作

            return {
                "status": "ready_to_apply",
                "region_id": region_id,
                "current_hash": current_hash,
            }

        except com_error as e:
            logger.error("apply_modification COM 错误: %s", e)
            return {"status": "error", "reason": str(e)}

    def apply_ooxml(
        self,
        region_id: str,
        new_ooxml: str,
        doc_path: str = "",
        expected_hash: str = "",
    ) -> dict:
        """
        直接将构建好的 OOXML 写入书签位置。
        这是实际的写入操作。
        """
        self._check_connection()

        try:
            doc = self._find_doc_by_path(doc_path)
            if doc is None:
                return {"status": "error", "reason": "无活动文档"}

            try:
                bookmark = doc.Bookmarks(region_id)
            except com_error:
                return {"status": "not_found", "reason": "书签不存在"}

            target_range = bookmark.Range
            if expected_hash and text_hash(str(target_range.Text)) != expected_hash:
                return {"status": "conflict", "reason": "原文已变化，请重新获取选区"}

            # 静默替换（InsertXML 不触发 UI 事件）
            target_range.InsertXML(new_ooxml)

            # 清理书签
            try:
                bookmark.Delete()
            except com_error:
                pass

            return {"status": "applied", "region_id": region_id}

        except com_error as e:
            logger.error("apply_ooxml COM 错误: %s", e)
            return {"status": "error", "reason": str(e)}

    def apply_text(self, region_id: str, new_text: str, doc_path: str = "") -> dict:
        """直接替换书签范围内的文本（不经过 OOXML 转换）。"""
        self._check_connection()
        try:
            doc = self._find_doc_by_path(doc_path)
            if doc is None:
                return {"status": "error", "reason": "无活动文档"}
            try:
                bookmark = doc.Bookmarks(region_id)
            except com_error:
                return {"status": "not_found", "reason": "书签不存在"}
            bookmark.Range.Text = new_text
            try:
                bookmark.Delete()
            except com_error:
                pass
            return {"status": "applied", "region_id": region_id}
        except com_error as e:
            logger.error("apply_text COM 错误: %s", e)
            return {"status": "error", "reason": str(e)}

    def apply_patch(self, region_id: str, patches: list[dict], doc_path: str = "", expected_hash: str = "") -> dict:
        """Preflight bounded, non-overlapping anchors; edit source ranges in reverse order."""
        self._check_connection()
        applied = 0
        try:
            doc = self._find_doc_by_path(doc_path)
            if doc is None:
                return {"status": "not_found", "reason": "源文档不存在"}
            bookmark = doc.Bookmarks(region_id)
            original = bookmark.Range.Duplicate
            initial_hash = text_hash(str(original.Text))
            if expected_hash and initial_hash != expected_hash:
                return {"status": "conflict", "reason": "原文已变化，请重新获取选区"}
            targets = []
            for index, patch in enumerate(patches):
                find_text, replacement = patch.get("find"), patch.get("replace")
                if not isinstance(find_text, str) or not find_text or not isinstance(replacement, str):
                    return {"status": "error", "reason": "patch 需要非空 find 和字符串 replace", "patch_index": index}
                rng = original.Duplicate
                finder = rng.Find
                finder.ClearFormatting()
                finder.Text = find_text
                finder.Forward = True
                finder.Wrap = 0
                finder.Format = False
                finder.MatchCase = True
                finder.MatchWildcards = False
                if not finder.Execute():
                    return {"status": "not_found", "patch_index": index, "patches_applied": 0}
                if str(rng.Text) != find_text:
                    return {"status": "conflict", "reason": "查找不是精确文本匹配", "patch_index": index}
                tail = original.Duplicate
                tail.SetRange(rng.End, original.End)
                more = tail.Find
                more.ClearFormatting()
                if more.Execute(FindText=find_text, Forward=True, Wrap=0, Format=False,
                                MatchCase=True, MatchWildcards=False):
                    return {"status": "conflict", "reason": "查找文本不唯一", "patch_index": index}
                targets.append((rng, find_text, replacement))
            targets.sort(key=lambda target: target[0].Start, reverse=True)
            for right, left in zip(targets, targets[1:]):
                if left[0].End > right[0].Start:
                    return {"status": "conflict", "reason": "patch 范围重叠"}
            if text_hash(str(bookmark.Range.Text)) != initial_hash:
                return {"status": "conflict", "reason": "预检期间原文已变化"}
            for rng, find_text, replacement in targets:
                prefix = 0
                for before, after in zip(find_text, replacement):
                    if before != after:
                        break
                    prefix += 1
                suffix = 0
                limit = min(len(find_text), len(replacement)) - prefix
                while suffix < limit and find_text[-suffix - 1] == replacement[-suffix - 1]:
                    suffix += 1
                old_end = len(find_text) - suffix
                new_end = len(replacement) - suffix
                # Word positions count UTF-16 code units, Python indexes code points.
                prefix_units = len(find_text[:prefix].encode("utf-16-le")) // 2
                suffix_units = len(find_text[len(find_text) - suffix:].encode("utf-16-le")) // 2 if suffix else 0
                anchor = rng.Start + prefix_units
                rng.SetRange(anchor, rng.End - suffix_units)
                if old_end > prefix:
                    rng.Delete()
                insertion = replacement[prefix:new_end]
                if insertion:
                    rng.SetRange(anchor, anchor)
                    rng.InsertAfter(insertion)
                applied += 1
            try:
                bookmark.Delete()
            except com_error:
                pass
            return {"status": "applied", "region_id": region_id, "patches_applied": applied}
        except com_error:
            return {"status": "partial_failed" if applied else "error",
                    "patches_applied": applied, "reason": "COM 写入失败，请检查文档后重新获取选区"}

    def check_region_state(self, region_id: str, doc_path: str = "") -> dict:
        """
        检查 Region 的当前状态（用于 diff 展示）。
        """
        self._check_connection()

        try:
            doc = self._find_doc_by_path(doc_path)
            if doc is None:
                return {"status": "no_document"}

            try:
                bookmark = doc.Bookmarks(region_id)
            except com_error:
                return {"status": "not_found", "reason": "书签已不存在"}

            current_text = str(bookmark.Range.Text)
            return {
                "status": "ok",
                "current_text": current_text,
                "current_hash": text_hash(current_text),
            }

        except com_error as e:
            return {"status": "error", "reason": str(e)}


# ══════════════════════════════════════════════
# COM STA Worker 线程
# ══════════════════════════════════════════════

class ComWorker(WorkerBase):
    """
    单线程 COM STA Worker。

    设计要点：
      - 独立线程 + STA 初始化
      - 任务队列（线程安全）序列化所有 COM 调用
      - 每个命令通过 Future 返回结果
      - COM 调用失败时自动重试（处理 RPC_E_CALL_REJECTED）
      - COM 消息泵维持线程活性

    （命令队列 / Future / 重试调度壳见 worker_base.WorkerBase）
    """

    @property
    def thread_name(self) -> str:
        return "WordBridge-COM-STA"

    def __init__(self, config: Optional[WordConfig] = None):
        self._handler: Optional[WordHandler] = None
        super().__init__(config or get_config().word)

    # ── Worker 主循环 ──

    def _worker_loop(self) -> None:
        """
        STA Worker 主循环。

        三步初始化：
          1. CoInitialize() — 初始化 COM STA
          2. 创建 WordHandler — 连接到 Word
          3. 进入事件循环 — 处理命令队列 + 泵送 COM 消息
        """
        # ─── 步骤 1: 初始化 COM STA ───
        try:
            pythoncom.CoInitialize()
        except Exception as e:
            logger.critical("CoInitialize 失败: %s", e)
            return

        try:
            # ─── 步骤 2: 连接 Word ───
            self._handler = WordHandler(prog_id=self.config.prog_id, config=self.config)
            # 启动时不再强制 Word 已运行；连不上就等按需连接
            self._handler.connect()

            logger.info("COM STA Worker 初始化完成，进入主循环")

            # ─── 步骤 3: 主循环 ───
            last_selection_poll = 0.0
            last_health_check = time.time()
            health_check_interval = self.config.health_check_interval

            while self._running:
                # (a) 处理命令队列（非阻塞）
                self._drain_command_queue()

                # (b) 泵送 COM 消息（防止 Word 假死）
                self._pump_com_messages()

                # The observer runs on this STA thread, including during stdio idle.
                now_monotonic = time.monotonic()
                interval = max(0.1, self.config.selection_poll_seconds)
                if not self._handler.is_connected:
                    interval = max(interval, 1.0)
                if now_monotonic - last_selection_poll >= interval:
                    self._handler.observe_selection()
                    last_selection_poll = time.monotonic()

                # (c) 定期健康检查
                now = time.time()
                if now - last_health_check > health_check_interval:
                    self._do_health_check()
                    last_health_check = now

                # (d) 微睡眠，避免 CPU 空转
                time.sleep(0.01)

        except Exception as e:
            logger.critical("COM Worker 主循环异常: %s", e, exc_info=True)

        finally:
            # ─── 清理 ───
            self._running = False
            while True:
                try:
                    pending = self._queue.get_nowait()
                except queue.Empty:
                    break
                if pending.future and pending.future.set_running_or_notify_cancel():
                    pending.future.set_result(ComResult(cmd_id=pending.cmd_id, success=False, error="COM Worker 已停止"))
            if self._handler:
                self._handler.disconnect()
                self._handler = None
            try:
                pythoncom.CoUninitialize()
            except Exception:
                pass
            logger.info("COM STA Worker 已退出")

    def _should_retry(self, e: Exception, attempt: int, cmd: ComCommand) -> bool:
        """COM 错误重试策略:交由 _handle_com_error 判定。"""
        if isinstance(e, com_error):
            return self._handle_com_error(e, attempt, cmd)
        logger.error("命令 %s 非 COM 错误: %s", cmd.cmd_type, e)
        return False

    def _handle_com_error(self, e: com_error, attempt: int, cmd: ComCommand) -> bool:
        """
        处理 COM 错误，决定是否重试。

        关键场景：
        - RPC_E_CALL_REJECTED (0x80010001): Word 正忙（模态框、长时间操作）
          → 重试
        - RPC_E_SERVER_DIED (0x800706BA): Word 进程崩溃
          → 不重试，标记连接失效
        - 其他: 根据错误类型判断
        """
        hresult = e.hresult

        # ── RPC_E_CALL_REJECTED — Word 正忙 ──
        if hresult == -2147418111:  # 0x80010001
            delay = self.config.com_retry_delay_ms * (
                self.config.com_retry_backoff ** attempt
            ) / 1000.0
            logger.debug(
                "RPC_E_CALL_REJECTED — Word 正忙，第 %d/%d 次重试，等待 %.1fs",
                attempt + 1, self.config.com_retry_max, delay
            )
            time.sleep(delay)
            # 额外泵送 COM 消息，帮助 Word 恢复
            self._pump_com_messages(msecs=int(delay * 500))
            return True

        # ── RPC_E_SERVER_DIED / CO_E_SERVER_EXEC_FAILURE ──
        if hresult in (-2147417848, -2146959359):
            logger.error("Word 进程可能已崩溃 (HRESULT: 0x%X)，停止重试", hresult & 0xFFFFFFFF)
            if self._handler:
                self._handler._connected = False
            return False

        # ── 其他 COM 错误：重试 1 次 ──
        if attempt < 1:
            logger.debug("COM 错误 (HRESULT: 0x%X)，尝试重试: %s", hresult & 0xFFFFFFFF, e)
            time.sleep(self.config.com_retry_delay_ms / 1000.0)
            return True

        return False

    def _pump_com_messages(self, msecs: int = 10) -> None:
        """泵送 COM 消息，保持 STA 线程活性。"""
        try:
            pythoncom.PumpWaitingMessages()
        except Exception:
            logger.exception("COM 消息泵失败")

    def _dispatch(self, cmd: ComCommand) -> Any:
        """分发命令到 WordHandler。在 STA 线程内执行。"""
        if self._handler is None:
            raise RuntimeError("WordHandler 未初始化")

        handler = self._handler

        if cmd.cmd_type == CmdType.HEALTH_CHECK:
            return handler.get_doc_info()

        elif cmd.cmd_type == CmdType.CAPTURE:
            return handler.capture_selected_ranges()

        elif cmd.cmd_type == CmdType.FIND_TEXT:
            return handler.find_text(
                text=cmd.payload["text"],
                max_matches=cmd.payload.get("max_matches", 20),
            )

        elif cmd.cmd_type == CmdType.READ_DOC:
            return handler.read_document(doc_path=cmd.payload.get("doc_path", ""))

        elif cmd.cmd_type == CmdType.APPLY:
            return handler.apply_ooxml(
                region_id=cmd.payload["region_id"],
                new_ooxml=cmd.payload["new_ooxml"],
                doc_path=cmd.payload.get("doc_path", ""),
                expected_hash=cmd.payload.get("expected_hash", ""),
            )

        elif cmd.cmd_type == CmdType.FORCE_APPLY:
            return handler.apply_ooxml(
                region_id=cmd.payload["region_id"],
                new_ooxml=cmd.payload["new_ooxml"],
                doc_path=cmd.payload.get("doc_path", ""),
            )

        elif cmd.cmd_type == CmdType.DIFF_CHECK:
            return handler.check_region_state(
                region_id=cmd.payload["region_id"],
                doc_path=cmd.payload.get("doc_path", ""),
            )

        elif cmd.cmd_type == CmdType.APPLY_TEXT:
            return handler.apply_text(
                region_id=cmd.payload["region_id"],
                new_text=cmd.payload["new_text"],
                doc_path=cmd.payload.get("doc_path", ""),
            )

        elif cmd.cmd_type == CmdType.APPLY_PATCH:
            return handler.apply_patch(
                region_id=cmd.payload["region_id"],
                patches=cmd.payload["patches"],
                doc_path=cmd.payload.get("doc_path", ""),
                expected_hash=cmd.payload.get("expected_hash", ""),
            )

        elif cmd.cmd_type == CmdType.GET_DOC_INFO:
            return handler.get_doc_info()

        elif cmd.cmd_type == CmdType.LIST_DOCS:
            return handler.list_documents()

        elif cmd.cmd_type == CmdType.OPEN_DOC:
            return handler.open_document(path=cmd.payload["path"])

        else:
            raise ValueError(f"未知命令类型: {cmd.cmd_type}")

    def _do_health_check(self) -> None:
        """定期健康检查。"""
        if self._handler is None:
            return
        try:
            info = self._handler.get_doc_info()
            if info.get("status") == "error":
                logger.warning("Word 健康检查异常")
        except Exception:
            pass
