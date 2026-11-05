"""
WebSocket / HTTP 服务模块
────────────────────────────────────────

与 MindLink 通信的桥接层。

协议设计：

  /mcp 使用 FastMCP 的标准 Streamable HTTP 传输。
  独立 WebSocket 端口保留为旧版调试接口：

  JSON-RPC 2.0 风格消息：
  {
    "jsonrpc": "2.0",
    "method": "tool_call",          # 或 "response"
    "id": "req_001",
    "params": {
      "name": "word_capture",       # 工具名
      "arguments": { ... }          # 参数
    }
  }

支持的工具（Tools）：
  word_capture:     等待并触发选区捕获
  word_modify:      推送 AI 修改并后台应用
  word_diff:        查询 Region 冲突状态
  word_force:       强制覆盖（忽略冲突）
  word_reject:      拒绝 AI 修改
  word_list:        列出所有活跃 Region
  word_status:      查询单个 Region 详情

WebSocket 端点：
  ws://127.0.0.1:9876/mcp       — 旧版调试 WebSocket

HTTP 端点（fallback / 调试）：
  GET  http://127.0.0.1:9877/health          — 健康检查
  GET  http://127.0.0.1:9877/regions         — 列出所有 Region
  GET  http://127.0.0.1:9877/regions/{id}    — Region 详情
  POST http://127.0.0.1:9877/capture         — 触发捕获（debug）
  POST http://127.0.0.1:9877/modify          — 应用修改（debug）
"""

from __future__ import annotations

import asyncio
import difflib
import hmac
import json
import os
import re
import time
import traceback
import uuid
from typing import Any, Callable, Coroutine, Optional

from websockets.asyncio.server import serve as websocket_serve
from websockets.exceptions import ConnectionClosed
import uvicorn
from starlette.requests import Request
from starlette.responses import JSONResponse, Response

from .com_worker import ComWorker, CmdType
from .config import ServerConfig, get_config
from .desensitize import desensitize, find_tokens, restore
from .ooxml_engine import OoxmlEngine
from .region_registry import RegionRegistry, RegionStatus
from .translator import TranslatorEngine, TranslationStateManager
from .translator.engine import TranslatorConfig
from .translator.state_manager import TaskStatus
from .utils import get_logger, text_hash, truncate
from .mcp_server import create_mcp_server


class _BearerTokenMiddleware:
    """Require ``Authorization: Bearer <token>`` on every HTTP route except /health.

    The host generates the token per launch and hands it only to its own MCP client, so
    other local processes and browser pages cannot drive Word through this port.
    """

    def __init__(self, app, token: str) -> None:
        self._app = app
        self._expected = f"Bearer {token}".encode()

    def __getattr__(self, name):
        return getattr(self._app, name)

    async def __call__(self, scope, receive, send) -> None:
        if scope["type"] == "http" and scope.get("path") != "/health":
            supplied = dict(scope.get("headers") or []).get(b"authorization", b"")
            if not hmac.compare_digest(supplied, self._expected):
                await send({"type": "http.response.start", "status": 401,
                            "headers": [(b"content-type", b"application/json")]})
                await send({"type": "http.response.body", "body": b'{"error":"unauthorized"}'})
                return
        await self._app(scope, receive, send)

logger = get_logger("ws_server")


# MCP schemas are generated from typed functions in mcp_server.py.

# ══════════════════════════════════════════════
# Word Bridge Service — 业务逻辑
# ══════════════════════════════════════════════

class WordBridgeService:
    """
    封装与 Word 交互的所有业务逻辑。

    不处理网络层，只处理数据转换和流程编排。
    供 WebSocket handler 和 HTTP handler 共同使用。
    """

    def __init__(
        self,
        com_worker: ComWorker,
        registry: RegionRegistry,
        ooxml_engine: OoxmlEngine,
    ):
        self.com = com_worker
        self.registry = registry
        self.ooxml = ooxml_engine

        # 出站脱敏 token→明文映射缓存（region_id → {token: original}）
        self._desensitize_maps: dict[str, dict[str, str]] = {}

        # 翻译引擎（延迟初始化，避免循环依赖）
        self._translator: Optional[TranslatorEngine] = None

    @property
    def translator(self) -> TranslatorEngine:
        """懒加载翻译引擎（从 config.yaml 的 translator 节读取配置）。"""
        if self._translator is None:
            import os
            from pathlib import Path
            import yaml

            # 直接从 YAML 加载 translator 配置节（避免改 Config dataclass）
            cfg = get_config()
            db_path = cfg.regions.database

            config_path = Path(__file__).resolve().parent.parent / "config.yaml"
            raw = {}
            if config_path.exists():
                with open(config_path, 'r', encoding='utf-8') as f:
                    raw = yaml.safe_load(f) or {}
            t_raw = raw.get("translator", {})

            translator_config = TranslatorConfig(
                provider=t_raw.get("provider", "kimi"),
                model=t_raw.get("model", "kimi-k3"),
                api_key=t_raw.get("api_key", ""),
                api_key_env=t_raw.get("api_key_env", "KIMI_API_KEY"),
                api_base=t_raw.get("api_base", "https://api.moonshot.cn/v1"),
                fallback_provider=t_raw.get("fallback_provider", ""),
                fallback_model=t_raw.get("fallback_model", ""),
                fallback_api_key=t_raw.get("fallback_api_key", ""),
                fallback_api_key_env=t_raw.get("fallback_api_key_env", ""),
                fallback_api_base=t_raw.get("fallback_api_base", ""),
                chunk_target_tokens=t_raw.get("chunk_target_tokens", 2000),
                chunk_max_tokens=t_raw.get("chunk_max_tokens", 2400),
                overlap_paragraphs=t_raw.get("overlap_paragraphs", 1),
                concurrency=t_raw.get("concurrency", 2),
                max_retries_per_chunk=t_raw.get("max_retries_per_chunk", 5),
                retry_base_delay=t_raw.get("retry_base_delay_seconds", 1.0),
                request_timeout=t_raw.get("request_timeout_seconds", 120.0),
                temperature=t_raw.get("temperature", 0.1),
                domain=t_raw.get("domain", "legal"),
            )
            state_mgr = TranslationStateManager(db_path=db_path)
            self._translator = TranslatorEngine(
                state_manager=state_mgr,
                config=translator_config,
            )
        return self._translator

    # ── 出站脱敏 / 写回还原 ──

    async def _desensitize(self, region) -> tuple[str, bool]:
        """对 Region 明文脱敏，缓存 token→明文映射到 self._desensitize_maps。

        返回 (展示文本, 是否脱敏)。失败时阻止明文进入工具响应。
        """
        text = region.text or ""
        if not text:
            return text, False

        result = await desensitize(text)
        if not (result.get("processed") or result.get("masked")) or not result.get("masked_text"):
            raise RuntimeError("脱敏网关未确认处理成功，请重试；未向模型返回原文")

        mapping = {
            e["token"]: e["original"]
            for e in result.get("entities", [])
            if e.get("token") and e.get("original")
        }
        if mapping:
            self._desensitize_maps.setdefault(region.region_id, {}).update(mapping)
            metadata = dict(getattr(region, "metadata", {}) or {})
            metadata["desensitize_map"] = mapping
            self.registry.update(region.region_id, metadata=metadata)
        return result["masked_text"], bool(result.get("masked"))

    async def _outbound_text(self, region_id: str, text):
        if not text:
            return text
        from types import SimpleNamespace
        display, _ = await self._desensitize(SimpleNamespace(region_id=region_id, text=text))
        return display

    def _restore_text(self, region_id: str, text: str) -> str:
        """写回前还原 token；存在无法还原的 token 时拒绝写入。"""
        if not text:
            return text
        mapping = self._desensitize_maps.get(region_id) or {}
        if not mapping:
            region = self.registry.get(region_id)
            if region is not None:
                saved = (region.metadata or {}).get("desensitize_map")
                if isinstance(saved, dict):
                    mapping = {
                        str(token): str(original)
                        for token, original in saved.items()
                        if token and original
                    }
                    if mapping:
                        self._desensitize_maps[region_id] = mapping
        restored = restore(text, mapping)
        leftover = find_tokens(restored)
        if leftover:
            raise ValueError("写回含无法还原的 token，请重新获取选区后重试")
        return restored

    def _minimal_patches(self, original: str, new_text: str) -> list:
        """对原文与新文本做字符级 diff，返回最小 find/replace 补丁列表。

        只把真正变化的部分作为补丁，未变部分保持不动（最小化修订）。
        """
        if not original or original == new_text:
            return []
        sm = difflib.SequenceMatcher(None, original, new_text)
        patches = []
        for op, a1, a2, b1, b2 in sm.get_opcodes():
            if op == "equal":
                continue
            if op == "replace":
                patches.append({"find": original[a1:a2], "replace": new_text[b1:b2]})
            elif op == "delete":
                patches.append({"find": original[a1:a2], "replace": ""})
            elif op == "insert":
                anchor = original[max(0, a1 - 1):a1]
                patches.append({"find": anchor, "replace": anchor + new_text[b1:b2]})
        return patches

    async def _capture_entry(self, region) -> dict:
        """构建 word_capture 返回的单条 region（text 已脱敏）。"""
        display_text, masked = await self._desensitize(region)
        return {
            "region_id": region.region_id,
            "text": display_text,
            "char_count": len(display_text) if display_text else 0,
            "preview": truncate(display_text, 200) if display_text else "",
            "doc_path": region.doc_path,
            "hash": region.text_hash,
            "masked": masked,
        }

    # ── Tool: word_capture ──

    async def handle_capture(self, timeout_ms: int = 30000) -> dict:
        """
        触发选区捕获。

        由于热键由 HotkeyHook 独立处理并通过回调触发捕获，
        这里的 capture 是一个「同步等待」模式：
        - 如果已有未处理的 Region，直接返回最新的一条
        - 否则等待至超时

        实际用法：MindLink 调用 word_capture → 用户按热键 → Region 进入 registry
                  → MindLink 轮询拿到结果
        """
        # 先查看是否有尚未被 MindLink 处理的新 Region
        pending = [
            r for r in self.registry.list_by_status(RegionStatus.CAPTURED)
        ]
        if pending:
            # 取最新的一条
            latest = max(pending, key=lambda r: r.captured_at)

            # 立即标记为 processing
            self.registry.transition(latest.region_id, RegionStatus.PROCESSING)

            return {
                "status": "ok",
                "regions": [await self._capture_entry(latest)],
            }

        # 没有待处理的 Region — 等待新捕获
        # 这里用轮询实现（最多等 timeout_ms）
        start = time.time()
        poll_interval = 0.1  # 100ms 轮询

        while (time.time() - start) * 1000 < timeout_ms:
            pending = [
                r for r in self.registry.list_by_status(RegionStatus.CAPTURED)
            ]
            if pending:
                latest = max(pending, key=lambda r: r.captured_at)
                self.registry.transition(latest.region_id, RegionStatus.PROCESSING)
                return {
                    "status": "ok",
                    "regions": [await self._capture_entry(latest)],
                }
            await asyncio.sleep(poll_interval)

        return {
            "status": "timeout",
            "message": f"在 {timeout_ms}ms 内未捕获到选区，请在 Word 中选中文字后按 Ctrl+Shift+H",
        }

    # ── Tool: word_modify ──

    async def handle_modify(self, region_id: str, new_text: str) -> dict:
        """
        应用 AI 修改到 Word。

        流程：
          1. 从 Registry 获取原始 OOXML
          2. 用 OoxmlEngine 构建新 OOXML
          3. 通过 ComWorker 检查当前文本状态
          4. 如无冲突，静默应用
          5. 如有冲突，返回冲突详情
        """
        region = self.registry.get(region_id)
        if region is None:
            return {"status": "error", "message": f"Region {region_id} 不存在"}

        # 写回前还原 token → 明文（本地映射）
        new_text = self._restore_text(region_id, new_text)

        # 保存 AI 结果
        self.registry.update(
            region_id,
            ai_text=new_text,
            status=RegionStatus.READY,
            processed_at=time.time(),
        )

        # 检查 Region 在 Word 中的当前状态
        future = self.com.diff_check(region_id, region.doc_path)
        try:
            result = await asyncio.wrap_future(future)
        except Exception as e:
            logger.error("diff_check 失败: %s", e)
            return {"status": "error", "message": f"检查 Region 状态失败: {e}"}

        if not result.success:
            return {"status": "error", "message": f"COM 错误: {result.error}"}

        diff_data = result.data
        if diff_data.get("status") == "not_found":
            self.registry.transition(region_id, RegionStatus.EXPIRED)
            return {
                "status": "region_gone",
                "message": "Region 已不存在（可能在 AI 处理期间被用户删除）",
            }

        current_text = diff_data.get("current_text", "")
        current_hash = diff_data.get("current_hash", "")

        # 冲突检测
        if current_hash != region.text_hash:
            # 计算相似度
            similarity = self.registry.compute_similarity(region.text, current_text)
            threshold = self.registry.config.conflict_similarity_threshold

            if similarity < threshold:
                # 大幅修改 → 返回冲突让用户决策
                self.registry.update(
                    region_id,
                    status=RegionStatus.CONFLICT,
                    conflict_text=current_text,
                    current_hash=current_hash,
                )
                return {
                    "status": "conflict",
                    "similarity": round(similarity, 3),
                    "original_text": await self._outbound_text(region_id, region.text),
                    "current_text": await self._outbound_text(region_id, current_text),
                    "ai_text": await self._outbound_text(region_id, new_text),
                    "message": (
                        "用户在 AI 处理期间大幅修改了原文（相似度 "
                        f"{similarity:.1%}）。请选择：[word_force] 覆盖 "
                        "| [word_reject] 保留当前"
                    ),
                }
            else:
                # 小修改 → 也返回冲突，但提示相似度较高
                self.registry.update(
                    region_id,
                    status=RegionStatus.CONFLICT,
                    conflict_text=current_text,
                    current_hash=current_hash,
                )
                return {
                    "status": "conflict",
                    "similarity": round(similarity, 3),
                    "original_text": await self._outbound_text(region_id, region.text),
                    "current_text": await self._outbound_text(region_id, current_text),
                    "ai_text": await self._outbound_text(region_id, new_text),
                    "message": (
                        f"用户做了小修改（相似度 {similarity:.1%}）。"
                        "建议用 [word_force] 覆盖或 [word_reject] 保留当前。"
                    ),
                }

        # ── 无冲突：字符级 diff → 最小修订写回（只动变化的部分）──
        patches = self._minimal_patches(region.text, new_text)
        if patches:
            try:
                result = await self.apply_patch(region_id, patches)
            except Exception as e:
                return {"status": "error", "message": f"应用失败: {e}"}
            if result.success and result.data.get("status") == "applied":
                return {
                    "status": "applied",
                    "region_id": region_id,
                    "patches": len(patches),
                    "message": "最小修订已应用到 Word",
                }
            return {"status": "error", "message": result.error or result.data.get("reason", "应用失败")}

        # 降级：无 diff（如原文为空）时走 OoxmlEngine 整段替换
        try:
            new_ooxml = self.ooxml.transform(
                original_ooxml=region.ooxml,
                new_text=new_text,
            )
        except Exception as e:
            logger.error("OoxmlEngine 转换失败: %s", e)
            return {"status": "error", "message": f"OOXML 构建失败: {e}"}

        # 保存构建好的 OOXML
        self.registry.update(region_id, ai_ooxml=new_ooxml)

        # 通过 ComWorker 应用
        future = self.com.apply(region_id, new_ooxml, doc_path=region.doc_path, expected_hash=region.text_hash)
        try:
            result = await asyncio.wrap_future(future)
        except Exception as e:
            return {"status": "error", "message": f"应用失败: {e}"}

        if result.success and result.data.get("status") == "applied":
            self.registry.update(
                region_id,
                status=RegionStatus.APPLIED,
                applied_at=time.time(),
            )
            self._desensitize_maps.pop(region_id, None)
            return {
                "status": "applied",
                "region_id": region_id,
                "message": "修改已成功应用到 Word",
            }
        else:
            return {
                "status": "error",
                "message": f"应用失败: {result.data.get('reason', result.error)}",
            }

    # ── Tool: word_force ──

    async def handle_force(self, region_id: str, new_text: str) -> dict:
        """强制应用修改，忽略冲突。OOXML 优先，失败则回退纯文本。"""
        region = self.registry.get(region_id)
        if region is None:
            return {"status": "error", "message": f"Region {region_id} 不存在"}

        new_text = self._restore_text(region_id, new_text)
        self.registry.update(region_id, ai_text=new_text)

        # Try OOXML first
        if region.ooxml:
            try:
                new_ooxml = self.ooxml.transform(region.ooxml, new_text)
                future = self.com.apply(region_id, new_ooxml, force=True, doc_path=region.doc_path)
                result = await asyncio.wrap_future(future)
                if result.success and result.data.get("status") == "applied":
                    self.registry.update(region_id, status=RegionStatus.APPLIED, applied_at=time.time())
                    return {"status": "applied", "region_id": region_id, "method": "ooxml"}
            except Exception:
                pass  # fall through to text mode

        # Fallback: plain text replace via bookmark range
        future = self.com.apply_text(region_id, new_text, doc_path=region.doc_path)
        try:
            result = await asyncio.wrap_future(future)
        except Exception as e:
            return {"status": "error", "message": f"应用失败: {e}"}

        if result.success and result.data.get("status") == "applied":
            self.registry.update(region_id, status=RegionStatus.APPLIED, applied_at=time.time())
            return {"status": "applied", "region_id": region_id, "method": "text"}
        else:
            return {"status": "error", "message": f"应用失败: {result.data.get('reason', result.error)}"}

    # ── Tool: word_diff ──

    async def handle_diff(self, region_id: str) -> dict:
        """查询冲突 diff。"""
        region = self.registry.get(region_id)
        if region is None:
            return {"status": "error", "message": f"Region {region_id} 不存在"}

        if region.status != RegionStatus.CONFLICT:
            return {
                "status": "ok",
                "region_status": region.status.value,
                "message": "该 Region 没有冲突",
            }

        return {
            "status": "conflict",
            "region_id": region_id,
            "original_text": await self._outbound_text(region_id, region.text),
            "current_text": await self._outbound_text(region_id, region.conflict_text),
            "ai_text": await self._outbound_text(region_id, region.ai_text),
            "original_hash": region.text_hash,
            "current_hash": region.current_hash,
        }

    # ── Tool: word_reject ──

    async def handle_reject(self, region_id: str) -> dict:
        """拒绝 AI 修改。"""
        region = self.registry.get(region_id)
        if region is None:
            return {"status": "error", "message": f"Region {region_id} 不存在"}

        self.registry.transition(region_id, RegionStatus.REJECTED)
        self._desensitize_maps.pop(region_id, None)
        return {"status": "rejected", "region_id": region_id}

    # ── Patch 写回（脱敏还原后走现有 Range.Delete + TypeText） ──

    async def apply_patch(self, region_id: str, patches: list) -> Any:
        """精准修订写回：先把 patch 里的 token 按本地映射还原成明文，
        再交给 com_worker.apply_patch 的 Range.Delete + TypeText（原逻辑不变）。"""
        region = self.registry.get(region_id)
        if region is None:
            raise ValueError("选区不存在或已过期，请重新获取")
        if patches:
            patches = [
                {
                    "find": self._restore_text(region_id, p.get("find", "")),
                    "replace": self._restore_text(region_id, p.get("replace", "")),
                }
                for p in patches
            ]
        doc_path = region.doc_path if region else ""
        future = self.com.apply_patch(region_id, patches, doc_path=doc_path, expected_hash=region.text_hash)
        result = await asyncio.wrap_future(future)
        if result.success and result.data.get("status") == "applied":
            self._desensitize_maps.pop(region_id, None)
            self.registry.update(region_id, status=RegionStatus.APPLIED, applied_at=time.time())
        return result

    async def handle_patch(self, region_id: str, patches: list) -> dict:
        """MCP word_patch 工具入口：包装 apply_patch 结果为可序列化 dict。"""
        if not region_id or not patches:
            return {"status": "error", "message": "缺少 region_id 或 patches"}
        try:
            result = await self.apply_patch(region_id, patches)
        except Exception as e:
            return {"status": "error", "message": str(e)}
        if result.success:
            return {"status": "ok", "region_id": region_id, **result.data}
        return {"status": "error", "message": result.error or "未知错误"}

    # ── Tool: word_get_active_selection ──

    async def handle_get_selection(self) -> dict:
        """主动拉取 Word 当前选区（绕过热键，COM 直接读取）。"""
        future = self.com.submit(CmdType.CAPTURE)
        try:
            result = await asyncio.wrap_future(future)
        except Exception as e:
            return {"status": "error", "message": f"COM 调用失败: {e}"}
        
        if not result.success:
            return {"status": "error", "message": f"捕获失败: {result.error or '无选区'}"}
        
        captured_list = result.data or []
        regions = []
        for captured in captured_list:
            # Use registry.create which stores full capture data (text + ooxml)
            region = self.registry.create(captured)
            display_text, masked = await self._desensitize(region)
            regions.append({
                "region_id": region.region_id,
                "text": display_text,
                "char_count": len(display_text) if display_text else 0,
                "preview": display_text[:200] if display_text else "",
                "has_ooxml": bool(region.ooxml),
                "source": captured.get("source", "active"),
                "masked": masked,
            })
        
        return {
            "status": "ok",
            "count": len(regions),
            "regions": regions,
        }

    # ── Tool: word_find ──

    async def handle_find(self, text: str, max_matches: int = 20) -> dict:
        """在文档中查找 text 并捕获为 region（仅定位不修改，后续用 word_patch）。"""
        if not text:
            return {"status": "error", "message": "缺少查找文本 text"}
        try:
            result = await asyncio.wrap_future(self.com.find_text(text, max_matches))
        except Exception as e:
            return {"status": "error", "message": f"COM 调用失败: {e}"}

        if not result.success:
            return {"status": "error", "message": f"查找失败: {result.error or '未知错误'}"}

        captured_list = result.data or []
        regions = []
        for captured in captured_list:
            region = self.registry.create(captured)
            display_text, masked = await self._desensitize(region)
            regions.append({
                "region_id": region.region_id,
                "text": display_text,
                "char_count": len(display_text) if display_text else 0,
                "preview": display_text[:200] if display_text else "",
                "has_ooxml": bool(region.ooxml),
                "source": captured.get("source", "find"),
                "masked": masked,
            })

        return {
            "status": "ok",
            "count": len(regions),
            "regions": regions,
        }

    # ── Tool: word_create_document ──

    async def handle_create_document(
        self,
        title: str = "",
        content: str = "",
        filename: str = "",
    ) -> dict:
        """用 Markdown 风格内容生成 .docx，并在 Word 中打开。"""
        if not content or not content.strip():
            return {"status": "error", "message": "缺少文档内容 content"}

        try:
            from pathlib import Path

            from docx import Document

            doc = Document()

            if title and title.strip():
                doc.add_heading(title.strip(), level=0)

            for line in content.splitlines():
                stripped = line.rstrip()
                if not stripped.strip():
                    continue
                if stripped.startswith("### "):
                    doc.add_heading(stripped[4:].strip(), level=3)
                elif stripped.startswith("## "):
                    doc.add_heading(stripped[3:].strip(), level=2)
                elif stripped.startswith("# "):
                    doc.add_heading(stripped[2:].strip(), level=1)
                elif stripped.startswith("- ") or stripped.startswith("* "):
                    doc.add_paragraph(stripped[2:].strip(), style="List Bullet")
                elif re.match(r"^\d+[.)]\s+", stripped):
                    doc.add_paragraph(re.sub(r"^\d+[.)]\s+", "", stripped), style="List Number")
                else:
                    doc.add_paragraph(stripped)

            base_dir = Path.home() / "Documents"
            base_dir.mkdir(parents=True, exist_ok=True)
            if filename and filename.strip():
                # 只取文件名部分，保证生成的文档始终落在 ~/Documents 内。
                name = re.sub(r'[\\/:*?"<>|]', "_", filename.strip()).lstrip(".") or "document"
                if not name.lower().endswith(".docx"):
                    name += ".docx"
            else:
                safe_title = re.sub(r'[\\/:*?"<>|]', "_", (title or "").strip()) or "document"
                name = f"{safe_title}.docx"
            save_path = base_dir / name

            doc.save(str(save_path))

            opened = False
            try:
                open_result = await asyncio.wrap_future(self.com.open_document(str(save_path)))
                opened = bool(
                    open_result.success
                    and (open_result.data or {}).get("status") == "ok"
                )
            except Exception as e:
                logger.warning("生成后打开 Word 失败: %s", e)

            return {
                "status": "ok",
                "path": str(save_path),
                "name": name,
                "opened": opened,
            }
        except Exception as e:
            return {"status": "error", "message": f"生成文档失败: {e}"}

    # ── Tool: word_list ──

    async def handle_list(self) -> dict:
        """列出所有活跃 Region。"""
        active = self.registry.list_active()
        return {
            "status": "ok",
            "count": len(active),
            "regions": [
                {
                    "region_id": r.region_id,
                    "status": r.status.value,
                    "preview": truncate(await self._outbound_text(r.region_id, r.text), 100),
                    "char_count": len(r.text),
                    "doc_path": r.doc_path,
                    "captured_at": r.captured_at,
                }
                for r in sorted(active, key=lambda x: x.captured_at, reverse=True)
            ],
        }

    # ── Tool: word_status ──

    async def handle_status(self, region_id: str) -> dict:
        """查询单个 Region 详情。"""
        region = self.registry.get(region_id)
        if region is None:
            return {"status": "error", "message": f"Region {region_id} 不存在"}

        return {
            "status": "ok",
            "region": {
                "region_id": region.region_id,
                "status": region.status.value,
                "text": await self._outbound_text(region_id, region.text),
                "ai_text": await self._outbound_text(region_id, region.ai_text),
                "char_count": len(region.text),
                "doc_path": region.doc_path,
                "captured_at": region.captured_at,
                "processed_at": region.processed_at,
                "applied_at": region.applied_at,
                "version": region.version,
            },
        }

    async def handle_list_documents(self) -> dict:
        """列出当前打开的所有 Word 文档。"""
        try:
            result = await asyncio.wrap_future(self.com.list_documents())
        except Exception as exc:
            return {"status": "error", "message": f"COM 调用失败: {exc}"}
        if not result.success:
            return {"status": "error", "message": f"列出文档失败: {result.error or '未知错误'}"}
        documents = result.data or []
        return {"status": "ok", "count": len(documents), "documents": documents}

    # ── Tool: word_read_document ──

    async def handle_read_document(self, document_id: str = "") -> dict:
        """读取打开的 Word 文档正文，捕获为 region（仅读取，后续用 word_patch 最小修订）。"""
        MAX_READ_CHARS = 3000
        try:
            if not document_id:
                docs_result = await asyncio.wrap_future(self.com.list_documents())
                if not docs_result.success:
                    return {"status": "error", "message": f"列出文档失败: {docs_result.error or '未知错误'}"}
                docs = docs_result.data or []
                if not docs:
                    return {"status": "error", "message": "没有打开的 Word 文档"}
                if len(docs) > 1:
                    return {
                        "status": "error",
                        "message": "有多个打开的 Word 文档，请用 word_list_documents 查看并指定 document_id",
                        "documents": docs,
                    }
                document_id = docs[0].get("path", "")

            result = await asyncio.wrap_future(self.com.read_document(document_id))
        except Exception as e:
            return {"status": "error", "message": f"COM 调用失败: {e}"}

        if not result.success:
            return {"status": "error", "message": f"读取失败: {result.error or '未知错误'}"}

        data = result.data or {}
        if data.get("status") != "ok":
            return {"status": "error", "message": data.get("message", "读取失败")}

        capture = data.get("capture") or {}
        region = self.registry.create(capture)
        display_text, masked = await self._desensitize(region)
        full_len = len(display_text) if display_text else 0
        truncated = full_len > MAX_READ_CHARS
        out_text = display_text[:MAX_READ_CHARS] if truncated else display_text

        return {
            "status": "ok",
            "document_name": data.get("name", ""),
            "document_path": data.get("path", ""),
            "region_id": region.region_id,
            "text": out_text,
            "char_count": len(out_text) if out_text else 0,
            "full_char_count": full_len,
            "truncated": truncated,
            "masked": masked,
        }

    # ── Tool: translate_long_text ──

    async def handle_translate(
        self,
        text: str = "",
        domain: str = "legal",
        provider: Optional[str] = None,
        model: Optional[str] = None,
        task_id: Optional[str] = None,
        glossary: Optional[dict] = None,
    ) -> dict:
        """超长文本双语翻译（即发即忘：立即返回 task_id，后台异步执行）。"""
        import asyncio, hashlib
        
        # Check for existing completed translation
        if text:
            source_hash = hashlib.sha256(text.encode()).hexdigest()
            existing = self.translator.state.find_by_hash(source_hash)
            if existing:
                task = self.translator.state.get_task(existing)
                if task and task.status == TaskStatus.DONE:
                    result = await self.translator.status(existing)
                    return {
                        "status": "done",
                        "task_id": existing,
                        "completed_chunks": result.completed_chunks,
                        "total_chunks": result.total_chunks,
                        "message": "已存在完成的翻译",
                    }
        
        # Start background translation — engine creates its own task
        result_future = asyncio.get_event_loop().create_future()
        
        async def _bg_translate():
            try:
                result = await self.translator.translate(
                    text=text, domain=domain, provider=provider,
                    model=model, glossary=glossary,
                )
                result_future.set_result(result)
            except Exception as e:
                logger.error("后台翻译异常: %s", e)
                result_future.set_exception(e)
        
        asyncio.ensure_future(_bg_translate())
        
        # Wait briefly for task creation
        try:
            result = await asyncio.wait_for(result_future, timeout=2.0)
            # Completed quickly (small text)
            return {
                "status": result.status.value,
                "task_id": result.task_id,
                "completed_chunks": result.completed_chunks,
                "total_chunks": result.total_chunks,
                "chinese_only": result.chinese_only,
                "took_seconds": result.took_seconds,
                "error": result.error,
            }
        except asyncio.TimeoutError:
            # Still running — query state manager for latest task
            import sqlite3
            db_path = self.translator.state.db_path
            conn = sqlite3.connect(db_path)
            row = conn.execute(
                "SELECT task_id FROM translation_tasks ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
            conn.close()
            latest_id = row[0] if row else "unknown"
            return {
                "status": "running",
                "task_id": latest_id,
                "message": "翻译后台执行中，请用 translate_status 查询进度",
            }

    # ── Tool: translate_status ──

    async def handle_translate_status(self, task_id: str) -> dict:
        """查询翻译进度。"""
        result = await self.translator.status(task_id)
        return {
            "status": result.status.value,
            "task_id": result.task_id,
            "completed_chunks": result.completed_chunks,
            "total_chunks": result.total_chunks,
            "took_seconds": result.took_seconds,
            "error": result.error,
        }

    # ── Tool: translate_glossary ──

    async def handle_translate_glossary(
        self,
        action: str,
        source_term: str = "",
        target_term: str = "",
        domain: str = "general",
    ) -> dict:
        """管理翻译术语表。"""
        state_mgr = self.translator.state

        if action == "add":
            if not source_term or not target_term:
                return {"status": "error", "message": "source_term 和 target_term 必填"}
            state_mgr.upsert_glossary(source_term, target_term, domain)
            return {
                "status": "ok",
                "action": "add",
                "source_term": source_term,
                "target_term": target_term,
                "domain": domain,
            }

        elif action == "list":
            all_terms = state_mgr.get_glossary(domain)
            return {
                "status": "ok",
                "action": "list",
                "domain": domain,
                "count": len(all_terms),
                "glossary": all_terms,
            }

        elif action == "delete":
            if not source_term:
                return {"status": "error", "message": "source_term 必填"}
            state_mgr.delete_glossary(source_term, domain)
            return {
                "status": "ok",
                "action": "delete",
                "source_term": source_term,
                "domain": domain,
            }

        return {"status": "error", "message": f"未知 action: {action}"}

    # ── Tool 路由 ──

    TOOL_HANDLERS: dict[str, Callable[..., Coroutine]] = {}  # 在实例化后绑定

    def _init_handlers(self):
        """绑定工具处理器。"""
        self.TOOL_HANDLERS = {
            "word_capture": self.handle_capture,
            "word_modify": self.handle_modify,
            "word_force": self.handle_force,
            "word_diff": self.handle_diff,
            "word_reject": self.handle_reject,
            "word_list": self.handle_list,
            "word_list_documents": self.handle_list_documents,
            "word_status": self.handle_status,
            "word_patch": self.handle_patch,
            "word_get_active_selection": self.handle_get_selection,
            "translate_long_text": self.handle_translate,
            "translate_status": self.handle_translate_status,
            "translate_glossary": self.handle_translate_glossary,
        }


# ══════════════════════════════════════════════
# WebSocket 服务器
# ══════════════════════════════════════════════

class BridgeServer:
    """FastMCP HTTP server plus the legacy local debugging WebSocket."""

    def __init__(
        self,
        service: WordBridgeService,
        config: Optional[ServerConfig] = None,
    ):
        self.service = service
        self.config = config or get_config().server
        if not getattr(self.service, "TOOL_HANDLERS", None):
            self.service._init_handlers()
        self.mcp = create_mcp_server(service, get_config().bridge.version)
        self._ws_server = None
        self._http_app = None
        self._http_server: Optional[uvicorn.Server] = None
        self._http_task: Optional[asyncio.Task] = None
        self._addin_http_server: Optional[uvicorn.Server] = None
        self._addin_task: Optional[asyncio.Task] = None
        self._routes_registered = False
        self._running = False

    async def start(self) -> None:
        """Start FastMCP Streamable HTTP and the legacy debug WebSocket."""
        self._running = True
        app = self._build_http_app()
        uvicorn_config = uvicorn.Config(
            app,
            host=self.config.host,
            port=self.config.http_port,
            log_config=None,
            access_log=False,
        )
        self._http_server = uvicorn.Server(uvicorn_config)
        self._http_task = asyncio.create_task(
            self._http_server.serve(), name="wordbridge-mcp-http"
        )
        for _ in range(100):
            if self._http_server.started:
                break
            if self._http_task.done():
                await self._http_task
                raise RuntimeError("MCP HTTP 服务启动失败")
            await asyncio.sleep(0.05)
        else:
            self._http_server.should_exit = True
            await asyncio.gather(self._http_task, return_exceptions=True)
            raise TimeoutError("MCP HTTP 服务启动超时")

        logger.info(
            "MCP Streamable HTTP 已启动: http://%s:%d/mcp",
            self.config.host,
            self.config.http_port,
        )

        if not self.config.ws_port:
            logger.info("旧版调试 WebSocket 已禁用（ws_port = 0）")
            await self._start_addin_server()
            return

        try:
            self._ws_server = await websocket_serve(
                self._ws_handler,
                self.config.host,
                self.config.ws_port,
                max_size=self.config.max_message_size,
                ping_interval=30,
                ping_timeout=10,
            )
        except BaseException:
            await self.stop()
            raise
        logger.info(
            "旧版调试 WebSocket 已启动: ws://%s:%d",
            self.config.host,
            self.config.ws_port,
        )

        # 加载项 HTTPS 服务（仅 macOS：Word 无 COM，改经 Office 加载项驱动）
        await self._start_addin_server()

    async def _start_addin_server(self) -> None:
        """在 macOS 上启动承载 taskpane + /addin-ws 的 HTTPS 服务器（自签证书）。"""
        import sys

        if sys.platform == "win32":
            return
        hub = getattr(self.service.com, "addin_hub", None)
        if hub is None:
            return

        from .addin_server import (
            ADDIN_DIR,
            ADDIN_HOST,
            ADDIN_PORT,
            CERT_FILE,
            KEY_FILE,
            build_addin_app,
        )

        if not CERT_FILE.exists() or not KEY_FILE.exists():
            logger.warning("加载项 HTTPS 证书缺失，跳过加载项服务: %s", CERT_FILE)
            return

        hub.bind_loop(asyncio.get_running_loop())
        app = build_addin_app(ADDIN_DIR, hub)
        uvicorn_config = uvicorn.Config(
            app,
            host=ADDIN_HOST,
            port=ADDIN_PORT,
            ssl_certfile=str(CERT_FILE),
            ssl_keyfile=str(KEY_FILE),
            log_config=None,
            access_log=False,
        )
        self._addin_http_server = uvicorn.Server(uvicorn_config)
        self._addin_task = asyncio.create_task(
            self._addin_http_server.serve(), name="wordbridge-addin-https"
        )
        for _ in range(100):
            if self._addin_http_server.started:
                break
            if self._addin_task.done():
                await self._addin_task
                logger.warning("加载项 HTTPS 服务启动失败（端口 %d 可能被占用）", ADDIN_PORT)
                return
            await asyncio.sleep(0.05)
        logger.info(
            "加载项 HTTPS 服务已启动: https://%s:%d/taskpane.html",
            ADDIN_HOST,
            ADDIN_PORT,
        )

    async def _stop_addin_server(self) -> None:
        if self._addin_http_server:
            self._addin_http_server.should_exit = True
        if self._addin_task:
            await asyncio.gather(self._addin_task, return_exceptions=True)
            self._addin_task = None
            self._addin_http_server = None
            logger.info("加载项 HTTPS 服务已关闭")

    async def stop(self) -> None:
        """Gracefully stop both network listeners."""
        self._running = False
        if self._ws_server:
            self._ws_server.close()
            await self._ws_server.wait_closed()
            self._ws_server = None
            logger.info("WebSocket 服务已关闭")

        await self._stop_addin_server()

        if self._http_server:
            self._http_server.should_exit = True
        if self._http_task:
            await asyncio.gather(self._http_task, return_exceptions=True)
            self._http_task = None
            self._http_server = None
            logger.info("MCP HTTP 服务已关闭")

    async def _tool_schemas(self) -> list[dict]:
        """Expose SDK-generated schemas to the legacy WebSocket debugger."""
        return [
            tool.to_mcp_tool().model_dump(by_alias=True, exclude_none=True)
            for tool in await self.mcp.list_tools()
        ]

    async def _ws_handler(self, websocket) -> None:
        client_id = uuid.uuid4().hex[:6]
        logger.info("WebSocket 客户端已连接: %s", client_id)
        try:
            async for raw_message in websocket:
                try:
                    response = await self._handle_message(raw_message)
                    await websocket.send(json.dumps(response, ensure_ascii=False))
                except json.JSONDecodeError as exc:
                    await websocket.send(json.dumps({
                        "jsonrpc": "2.0",
                        "id": None,
                        "error": {"code": -32700, "message": f"JSON 解析失败: {exc}"},
                    }, ensure_ascii=False))
                except Exception as exc:
                    logger.error("消息处理异常: %s\n%s", exc, traceback.format_exc())
                    await websocket.send(json.dumps({
                        "jsonrpc": "2.0",
                        "id": None,
                        "error": {"code": -32603, "message": str(exc)},
                    }, ensure_ascii=False))
        except ConnectionClosed:
            logger.info("WebSocket 客户端已断开: %s", client_id)
        except Exception as exc:
            logger.error("WebSocket 异常: %s", exc)

    async def _handle_message(self, raw: str) -> dict:
        """Handle the retained non-MCP WebSocket debugging protocol."""
        msg = json.loads(raw)
        method = msg.get("method", "")
        msg_id = msg.get("id", uuid.uuid4().hex[:8])
        params = msg.get("params", {})

        if method == "ping":
            return {"jsonrpc": "2.0", "id": msg_id, "result": "pong"}
        if method == "list_tools":
            return {
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {"tools": await self._tool_schemas()},
            }
        if method == "tool_call":
            tool_name = params.get("name", "")
            handler = self.service.TOOL_HANDLERS.get(tool_name)
            if handler is None:
                return {
                    "jsonrpc": "2.0",
                    "id": msg_id,
                    "error": {"code": -32601, "message": f"未知工具: {tool_name}"},
                }
            try:
                result = await handler(**params.get("arguments", {}))
                return {"jsonrpc": "2.0", "id": msg_id, "result": result}
            except TypeError as exc:
                return {
                    "jsonrpc": "2.0",
                    "id": msg_id,
                    "error": {"code": -32602, "message": f"参数错误: {exc}"},
                }
            except Exception as exc:
                logger.error("工具 %s 执行异常: %s", tool_name, exc)
                return {
                    "jsonrpc": "2.0",
                    "id": msg_id,
                    "error": {"code": -32603, "message": str(exc)},
                }
        return {
            "jsonrpc": "2.0",
            "id": msg_id,
            "error": {"code": -32601, "message": f"未知方法: {method}"},
        }

    def _register_http_routes(self) -> None:
        if self._routes_registered:
            return
        routes = (
            ("/health", ["GET"], self._http_health),
            ("/regions", ["GET"], self._http_list_regions),
            ("/regions/{region_id}", ["GET"], self._http_get_region),
            ("/capture", ["POST"], self._http_capture),
            ("/modify", ["POST"], self._http_modify),
            ("/patch", ["POST"], self._http_patch),
            ("/force-capture", ["POST"], self._http_force_capture),
            ("/ooxml/{region_id}", ["GET"], self._http_get_ooxml),
            ("/translate", ["POST"], self._http_translate),
            ("/translate/{task_id}/status", ["GET"], self._http_translate_status),
            ("/translate/{task_id}/result", ["GET"], self._http_translate_result),
        )
        for path, methods, handler in routes:
            self.mcp.custom_route(path, methods)(handler)
        self._routes_registered = True

    def _build_http_app(self):
        """Build one ASGI app containing official MCP and debug REST routes."""
        if self._http_app is None:
            self._register_http_routes()
            self._http_app = self.mcp.http_app(
                path="/mcp",
                json_response=True,
                stateless_http=True,
                allowed_hosts=[
                    f"{self.config.host}:*",
                    "127.0.0.1:*",
                    "localhost:*",
                    "[::1]:*",
                ],
                allowed_origins=[
                    f"http://{self.config.host}:*",
                    "http://127.0.0.1:*",
                    "http://localhost:*",
                    "http://[::1]:*",
                ],
            )
            token = os.environ.get("WORDBRIDGE_AUTH_TOKEN", "").strip()
            if token:
                self._http_app = _BearerTokenMiddleware(self._http_app, token)
        return self._http_app

    async def _http_health(self, request: Request) -> Response:
        future = self.service.com.submit(CmdType.HEALTH_CHECK)
        try:
            result = await asyncio.wait_for(asyncio.wrap_future(future), timeout=5.0)
            return JSONResponse({
                "status": "ok",
                "word_connected": result.success,
                "regions_active": self.service.registry.count(),
                "mcp_transport": "streamable-http",
            })
        except asyncio.TimeoutError:
            return JSONResponse({
                "status": "degraded",
                "word_connected": False,
                "error": "COM Worker 响应超时",
                "mcp_transport": "streamable-http",
            }, status_code=503)

    async def _http_list_regions(self, request: Request) -> Response:
        return JSONResponse(await self.service.handle_list())

    async def _http_get_region(self, request: Request) -> Response:
        result = await self.service.handle_status(request.path_params["region_id"])
        return JSONResponse(result, status_code=200 if result["status"] == "ok" else 404)

    async def _http_get_ooxml(self, request: Request) -> Response:
        region = self.service.registry.get(request.path_params["region_id"])
        if region is None:
            return JSONResponse({"status": "error", "message": "region 不存在"}, status_code=404)
        return JSONResponse({"status": "ok", "region_id": region.region_id, "ooxml": getattr(region, "ooxml", "") or ""})

    @staticmethod
    async def _json_body(request: Request) -> dict:
        if not await request.body():
            return {}
        return await request.json()

    async def _http_capture(self, request: Request) -> Response:
        body = await self._json_body(request)
        return JSONResponse(await self.service.handle_capture(body.get("timeout_ms", 5000)))

    async def _http_modify(self, request: Request) -> Response:
        body = await self._json_body(request)
        region_id = body.get("region_id")
        new_text = body.get("new_text")
        if not region_id or new_text is None:
            return JSONResponse(
                {"status": "error", "message": "缺少 region_id 或 new_text"},
                status_code=400,
            )
        handler = self.service.handle_force if body.get("force", False) else self.service.handle_modify
        return JSONResponse(await handler(region_id, new_text))

    async def _http_patch(self, request: Request) -> Response:
        body = await self._json_body(request)
        region_id = body.get("region_id")
        patches = body.get("patches", [])
        if not region_id or not patches:
            return JSONResponse(
                {"status": "error", "message": "缺少 region_id 或 patches"},
                status_code=400,
            )
        try:
            result = await self.service.apply_patch(region_id, patches)
        except Exception as exc:
            return JSONResponse({"status": "error", "message": str(exc)}, status_code=500)
        if result.success:
            return JSONResponse(result.data)
        return JSONResponse({"status": "error", "message": result.error}, status_code=500)

    async def _http_force_capture(self, request: Request) -> Response:
        return JSONResponse(await self.service.handle_get_selection())

    async def _http_translate(self, request: Request) -> Response:
        body = await self._json_body(request)
        return JSONResponse(await self.service.handle_translate(
            text=body.get("text", ""),
            domain=body.get("domain", "legal"),
            provider=body.get("provider"),
            model=body.get("model"),
            task_id=body.get("task_id"),
            glossary=body.get("glossary"),
        ))

    async def _http_translate_status(self, request: Request) -> Response:
        return JSONResponse(
            await self.service.handle_translate_status(request.path_params["task_id"])
        )

    async def _http_translate_result(self, request: Request) -> Response:
        task_id = request.path_params["task_id"]
        result = await self.service.translator.status(task_id)
        if result.status.value == "done":
            full = await self.service.translator.translate(text="", task_id=task_id)
            return JSONResponse({
                "status": full.status.value,
                "task_id": full.task_id,
                "bilingual_text": full.bilingual_text,
                "chinese_only": full.chinese_only,
                "alignment": full.alignment,
                "took_seconds": full.took_seconds,
            })
        return JSONResponse({
            "status": result.status.value,
            "task_id": result.task_id,
            "completed_chunks": result.completed_chunks,
            "total_chunks": result.total_chunks,
        })
