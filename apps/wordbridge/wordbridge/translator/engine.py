"""
翻译引擎 — 流水线编排器

将 chunker、llm_client、state_manager、assembler 编排为完整的翻译流水线。

核心 API:
  engine.translate(text)        → 启动翻译 (立即返回 task_id，后台执行)
  engine.translate_and_wait()   → 同步等待翻译完成 (阻塞)
  engine.resume(task_id)        → 断点续传
  engine.status(task_id)        → 查询进度
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import time
from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Optional

from ..utils import get_logger
from .chunker import TextChunker, Chunk, ChunkResult, count_tokens
from .llm_client import (
    LLMClient,
    LLMConfig,
    LLMResponse,
    build_llm_config,
    PROVIDER_CONFIG,
)
from .state_manager import (
    TranslationStateManager,
    TaskStatus,
    ChunkStatus,
    ChunkRecord,
)
from .assembler import Assembler, AssemblyResult
from .prompts import get_system_prompt

logger = get_logger("translator.engine")


# ══════════════════════════════════════════════
# 数据模型
# ══════════════════════════════════════════════

class TranslationStatus(str, Enum):
    PENDING = "pending"
    RUNNING = "running"
    DONE = "done"
    PARTIAL_FAILED = "partial_failed"
    ABORTED = "aborted"


@dataclass
class TranslationResult:
    """翻译结果。"""
    task_id: str
    status: TranslationStatus
    completed_chunks: int = 0
    total_chunks: int = 0
    bilingual_text: Optional[str] = None
    chinese_only: Optional[str] = None
    alignment: list[dict] = field(default_factory=list)
    failed_chunk_indices: list[int] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    took_seconds: float = 0.0
    error: str = ""


@dataclass
class TranslatorConfig:
    """翻译引擎配置。"""
    # LLM
    provider: str = "kimi"
    model: str = ""
    api_key: str = ""           # API Key 直接值（优先级高于 api_key_env）
    api_key_env: str = ""       # 从环境变量读取
    api_base: str = ""
    fallback_provider: str = ""
    fallback_model: str = ""
    fallback_api_key: str = ""       # fallback API Key 直接值
    fallback_api_key_env: str = ""   # fallback 从环境变量读取
    fallback_api_base: str = ""

    # 分块
    chunk_target_tokens: int = 2000
    chunk_max_tokens: int = 2400
    overlap_paragraphs: int = 1

    # 并发 & 重试
    concurrency: int = 4
    max_retries_per_chunk: int = 3
    retry_base_delay: float = 1.0
    request_timeout: float = 120.0
    temperature: float = 0.1

    # 输出
    domain: str = "legal"


# ══════════════════════════════════════════════
# 编排器
# ══════════════════════════════════════════════

class TranslatorEngine:
    """
    翻译流水线编排器。

    用法:
        engine = TranslatorEngine(state_manager=..., config=...)
        result = await engine.translate_and_wait(text, domain="legal")
    """

    def __init__(
        self,
        state_manager: TranslationStateManager,
        config: Optional[TranslatorConfig] = None,
    ):
        self.state = state_manager
        self.config = config or TranslatorConfig()

    # ── 公共 API ──

    async def translate(
        self,
        text: str,
        domain: str = "legal",
        provider: Optional[str] = None,
        model: Optional[str] = None,
        task_id: Optional[str] = None,
        glossary: Optional[dict[str, str]] = None,
    ) -> TranslationResult:
        """
        启动翻译（异步，在调用线程中阻塞直到完成）。

        参数:
          text:      待翻译的英文全文
          domain:    领域 (legal | business | general)
          provider:  主 LLM Provider
          model:     主模型名
          task_id:   续传时传入已有的 task_id
          glossary:  自定义术语表

        返回: TranslationResult
        """
        # 0. 入参校验
        if not text or not text.strip():
            return TranslationResult(
                task_id="",
                status=TranslationStatus.DONE,
                error="文本为空",
            )
        if self._is_chinese_text(text):
            return TranslationResult(
                task_id="",
                status=TranslationStatus.DONE,
                error="文本已是中文，无需翻译",
            )

        # 1. 去重检查
        source_hash = hashlib.sha256(text.encode()).hexdigest()
        existing_task_id = self.state.find_by_hash(source_hash)
        if existing_task_id and not task_id:
            task = self.state.get_task(existing_task_id)
            if task and task.status == TaskStatus.DONE:
                logger.info("发现已完成的翻译 %s，直接返回", existing_task_id)
                return await self._build_done_result(existing_task_id)

        # 2. 创建或恢复任务
        if task_id:
            # 断点续传
            task = self.state.get_task(task_id)
            if task is None:
                return TranslationResult(
                    task_id=task_id,
                    status=TranslationStatus.DONE,
                    error=f"任务 {task_id} 不存在",
                )
            return await self._execute_translation(
                task_id=task_id,
                domain=domain,
                glossary=glossary,
            )
        else:
            # 新建任务
            return await self._execute_translation(
                task_id=None,
                text=text,
                source_hash=source_hash,
                domain=domain,
                provider=provider,
                model=model,
                glossary=glossary,
            )

    async def translate_and_wait(
        self,
        text: str,
        domain: str = "legal",
        provider: Optional[str] = None,
        model: Optional[str] = None,
        glossary: Optional[dict[str, str]] = None,
    ) -> TranslationResult:
        """翻译并等待完成（同上，语义更明确）。"""
        return await self.translate(
            text=text,
            domain=domain,
            provider=provider,
            model=model,
            glossary=glossary,
        )

    async def resume(self, task_id: str) -> TranslationResult:
        """断点续传。"""
        return await self.translate(text="", task_id=task_id)

    async def status(self, task_id: str) -> TranslationResult:
        """查询翻译进度。"""
        task = self.state.get_task(task_id)
        if task is None:
            return TranslationResult(
                task_id=task_id,
                status=TranslationStatus.DONE,
                error=f"任务 {task_id} 不存在",
            )

        return TranslationResult(
            task_id=task_id,
            status=TranslationStatus(task.status),
            completed_chunks=task.completed_chunks,
            total_chunks=task.total_chunks,
            took_seconds=time.time() - task.created_at,
        )

    # ── 核心执行 ──

    async def _execute_translation(
        self,
        task_id: Optional[str] = None,
        text: str = "",
        source_hash: str = "",
        domain: str = "legal",
        provider: Optional[str] = None,
        model: Optional[str] = None,
        glossary: Optional[dict[str, str]] = None,
    ) -> TranslationResult:
        """执行完整的翻译流水线。"""
        start_time = time.time()
        is_resume = task_id is not None

        # ── 新建任务: 分块 + 创建 Task ──
        if not is_resume:
            chunker = TextChunker(
                target_tokens=self.config.chunk_target_tokens,
                max_tokens=self.config.chunk_max_tokens,
                overlap=self.config.overlap_paragraphs,
            )
            chunk_result = chunker.chunk(text)

            if not chunk_result.chunks:
                return TranslationResult(
                    task_id="",
                    status=TranslationStatus.DONE,
                    error="分块结果为空",
                )

            # 创建 Task
            actual_provider = provider or self.config.provider
            actual_model = model or self.config.model
            task_id = self.state.create_task(
                source_hash=source_hash or hashlib.sha256(text.encode()).hexdigest(),
                total_chunks=len(chunk_result.chunks),
                provider=actual_provider,
                model=actual_model,
                domain=domain,
                config_snapshot={
                    "chunk_target": self.config.chunk_target_tokens,
                    "chunk_max": self.config.chunk_max_tokens,
                    "concurrency": self.config.concurrency,
                },
            )

            # 持久化 Chunks
            chunk_data = []
            for c in chunk_result.chunks:
                chunk_data.append({
                    "index": c.index,
                    "paragraphs": c.paragraphs,
                    "token_count": c.token_count,
                })
            self.state.save_chunks(task_id, chunk_data)

            # 上下文锚点映射 (chunk_index → overlap_prev)
            overlap_map = {
                c.index: c.overlap_prev
                for c in chunk_result.chunks
                if c.overlap_prev
            }
            pending_chunks = self.state.get_pending_chunks(task_id)
        else:
            # 断点续传
            pending_chunks = self.state.get_pending_chunks(task_id)
            if not pending_chunks:
                # 全部完成，直接返回
                self.state.update_task(task_id, status=TaskStatus.DONE)
                return await self._build_done_result(task_id)

            self.state.update_task(task_id, status=TaskStatus.RUNNING)
            # 从 DB 恢复 overlap 信息（续传时不需要 overlap，因为上下文已丢失）
            overlap_map = {}
            logger.info(
                "断点续传 %s: %d 个 Chunk 待处理，%d 已完成",
                task_id, len(pending_chunks),
                self.state.get_task(task_id).total_chunks - len(pending_chunks),
            )

        total_chunks = self.state.get_task(task_id).total_chunks

        # ── 构建 LLM Client ──
        llm = self._build_llm_client(provider, model)
        glossary_all = self._load_glossary(domain, glossary)

        # ── System Prompt ──
        system_prompt = get_system_prompt(domain)

        # ── 并发翻译 ──
        semaphore = asyncio.Semaphore(self.config.concurrency)
        failed_indices: list[int] = []

        async def process_chunk(chunk_rec: ChunkRecord) -> None:
            async with semaphore:
                try:
                    # 解析源段落
                    source_paras = json.loads(chunk_rec.source_json)
                    overlap_prev = overlap_map.get(chunk_rec.chunk_index)

                    # 调用 LLM
                    self.state.set_chunk_status(chunk_rec.chunk_id, ChunkStatus.PROCESSING)
                    resp = await llm.translate_chunk(
                        paragraphs=source_paras,
                        system_prompt=system_prompt,
                        overlap_prev=overlap_prev,
                        glossary=glossary_all,
                        domain=domain,
                    )

                    if resp.success:
                        self.state.mark_chunk_done(
                            chunk_rec.chunk_id,
                            translated_json=json.dumps(
                                resp.parsed_pairs, ensure_ascii=False
                            ),
                            provider=resp.provider_used,
                            model=resp.model_used,
                            latency_ms=resp.latency_ms,
                            tokens_used=resp.tokens_used,
                        )
                        # 更新 task 完成计数
                        current = self.state.get_task(task_id)
                        self.state.update_task(
                            task_id,
                            completed_chunks=current.completed_chunks + 1,
                        )
                        logger.info(
                            "Chunk %d/%d done (%dms)",
                            chunk_rec.chunk_index + 1, total_chunks, resp.latency_ms,
                        )
                    else:
                        raise Exception(resp.error)
                except Exception as e:
                    self.state.mark_chunk_failed(chunk_rec.chunk_id, str(e))
                    if chunk_rec.retry_count >= self.config.max_retries_per_chunk:
                        failed_indices.append(chunk_rec.chunk_index)
                        logger.error(
                            "Chunk %d 最终失败: %s", chunk_rec.chunk_index, e
                        )
                    else:
                        # 重置为 pending 以允许重试
                        self.state.set_chunk_status(chunk_rec.chunk_id, ChunkStatus.PENDING)

        # 执行所有 pending Chunks（包括重试的）
        max_rounds = self.config.max_retries_per_chunk + 1
        for round_num in range(max_rounds):
            pending = self.state.get_pending_chunks(task_id)
            if not pending:
                break

            logger.debug("第 %d 轮: 处理 %d 个 Chunk", round_num + 1, len(pending))
            tasks = [process_chunk(c) for c in pending]
            await asyncio.gather(*tasks, return_exceptions=True)

            if round_num == max_rounds - 1:
                # 最后一轮后仍失败的就是最终失败
                still_pending = self.state.get_pending_chunks(task_id)
                for c in still_pending:
                    self.state.mark_chunk_failed(
                        c.chunk_id,
                        f"重试 {max_rounds} 轮后仍失败",
                    )

        # ── 更新任务状态 ──
        final_pending = self.state.get_pending_chunks(task_id)
        if not final_pending and not failed_indices:
            self.state.update_task(task_id, status=TaskStatus.DONE)
        else:
            self.state.update_task(
                task_id,
                status=TaskStatus.PARTIAL_FAILED,
                failed_chunks=len(failed_indices or final_pending),
                error_summary=f"{len(failed_indices or final_pending)} 个 Chunk 翻译失败",
            )

        # ── 组装结果 ──
        elapsed = time.time() - start_time

        task_status = self.state.get_task(task_id).status
        if task_status == TaskStatus.DONE:
            assembler = Assembler(self.state)
            assembly = assembler.build(task_id)
            return TranslationResult(
                task_id=task_id,
                status=TranslationStatus.DONE,
                completed_chunks=total_chunks,
                total_chunks=total_chunks,
                bilingual_text=assembly.bilingual_text,
                chinese_only=assembly.chinese_only,
                alignment=[
                    {"index": a.index, "source": a.source, "translated": a.translated}
                    for a in assembly.alignment
                ],
                warnings=assembly.warnings,
                took_seconds=elapsed,
            )
        else:
            return TranslationResult(
                task_id=task_id,
                status=TranslationStatus.PARTIAL_FAILED,
                completed_chunks=total_chunks - len(final_pending or []),
                total_chunks=total_chunks,
                failed_chunk_indices=failed_indices,
                took_seconds=elapsed,
                error=f"{len(failed_indices or final_pending)} 个 Chunk 翻译失败，请使用 resume('{task_id}') 重试",
            )

    async def _build_done_result(self, task_id: str) -> TranslationResult:
        """从已完成的 Task 构建结果。"""
        task = self.state.get_task(task_id)
        assembler = Assembler(self.state)
        assembly = assembler.build(task_id)
        return TranslationResult(
            task_id=task_id,
            status=TranslationStatus.DONE,
            completed_chunks=task.completed_chunks,
            total_chunks=task.total_chunks,
            bilingual_text=assembly.bilingual_text,
            chinese_only=assembly.chinese_only,
            alignment=[
                {"index": a.index, "source": a.source, "translated": a.translated}
                for a in assembly.alignment
            ],
            warnings=assembly.warnings,
            took_seconds=task.updated_at - task.created_at,
        )

    # ── 辅助方法 ──

    def _build_llm_client(
        self,
        provider_override: Optional[str] = None,
        model_override: Optional[str] = None,
    ) -> LLMClient:
        """构建 LLMClient，支持运行时覆盖。API Key 优先级：直接值 > 环境变量。"""
        provider = provider_override or self.config.provider

        # 主 Provider: api_key 优先于环境变量
        primary_key = self.config.api_key or os.environ.get(self.config.api_key_env, "")

        primary = build_llm_config(
            provider=provider,
            model=model_override or self.config.model,
            api_key=primary_key,
            api_base=self.config.api_base,
            temperature=self.config.temperature,
            request_timeout=self.config.request_timeout,
            max_retries=self.config.max_retries_per_chunk,
        )

        fallback = None
        if self.config.fallback_provider:
            fallback_key = self.config.fallback_api_key or os.environ.get(
                self.config.fallback_api_key_env, ""
            )
            fallback = build_llm_config(
                provider=self.config.fallback_provider,
                model=self.config.fallback_model,
                api_key=fallback_key,
                api_base=self.config.fallback_api_base,
                temperature=self.config.temperature,
                request_timeout=self.config.request_timeout,
                max_retries=self.config.max_retries_per_chunk,
            )

        return LLMClient(primary_config=primary, fallback_config=fallback)

    def _load_glossary(
        self,
        domain: str,
        custom_glossary: Optional[dict[str, str]] = None,
    ) -> dict[str, str]:
        """加载术语表（DB + 自定义合并）。"""
        glossary = self.state.get_glossary(domain)
        if custom_glossary:
            glossary.update(custom_glossary)
        return glossary

    @staticmethod
    def _is_chinese_text(text: str) -> bool:
        """简单检测文本是否主要是中文（避免重复翻译）。"""
        if not text:
            return False
        cjk_count = sum(
            1 for c in text
            if '一' <= c <= '鿿' or '㐀' <= c <= '䶿'
        )
        total = len(text.replace(' ', '').replace('\n', ''))
        if total == 0:
            return False
        return cjk_count / total > 0.8
