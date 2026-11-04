"""
翻译引擎 — 滑窗分块算法 (Sliding Window Chunker)

核心原则:
  1. 分块边界必须在 \n\n（段落边界）处，绝不截断英文句子
  2. Token 计数用 tiktoken (cl100k_base)，近似 kimi/deepseek 的 tokenizer
  3. 每个 Chunk 目标 ~2000 tokens，硬上限 2400 tokens
  4. 超长段落 → 按句子边界二次切分
  5. Chunk 间重叠 1 段（上下文锚点，只发给 LLM 理解，不参与输出）
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Optional

from ..utils import get_logger

logger = get_logger("translator.chunker")

# ── Token 计数 ──

try:
    import tiktoken
    _ENCODER = tiktoken.get_encoding("cl100k_base")
    _HAS_TIKTOKEN = True
except ImportError:
    _ENCODER = None
    _HAS_TIKTOKEN = False
    logger.warning("tiktoken 不可用，token 计数将使用字符数估算 (×0.3 系数)")


def count_tokens(text: str) -> int:
    """精确 token 计数，降级为字符估算。"""
    if _HAS_TIKTOKEN and _ENCODER is not None:
        try:
            return len(_ENCODER.encode(text))
        except Exception:
            pass
    return int(len(text) * 0.3)


# ── 句子边界分割 ──

_SENTENCE_BOUNDARY = re.compile(
    r'(?<=[.!?])\s+(?=[A-Z"\'(])',
    re.MULTILINE,
)


def split_by_sentence(text: str) -> list[str]:
    """按句子边界切分英文文本。"""
    parts = _SENTENCE_BOUNDARY.split(text)
    if not parts:
        return [text]
    sentences = []
    for i, part in enumerate(parts):
        if i < len(parts) - 1:
            # 重新拼接被 split 移除的空格
            sentences.append(part + " ")
        else:
            sentences.append(part)
    return sentences or [text]


# ── 数据模型 ──

@dataclass
class Chunk:
    """单个翻译块。"""
    index: int                            # 0-based 序号
    paragraphs: list[str]                 # 该块的段落列表
    token_count: int                      # 该块的 token 总数
    overlap_prev: Optional[str] = None    # 上一块的最后一段（上下文锚点）


@dataclass
class ChunkResult:
    """分块结果。"""
    chunks: list[Chunk] = field(default_factory=list)
    total_paragraphs: int = 0
    total_tokens: int = 0
    warnings: list[str] = field(default_factory=list)


# ══════════════════════════════════════════════
# 分块算法
# ══════════════════════════════════════════════

class TextChunker:
    """
    滑窗分块器。

    参数：
      target_tokens: 每个 Chunk 的目标 token 数（默认 2000）
      max_tokens:    硬上限（默认 2400），超限段落触发句子级切分
      overlap:       Chunk 间重叠段落数（默认 1），用于上下文锚点
    """

    def __init__(
        self,
        target_tokens: int = 2000,
        max_tokens: int = 2400,
        overlap: int = 1,
    ):
        self.target = target_tokens
        self.max = max_tokens
        self.overlap = overlap

    def chunk(self, text: str) -> ChunkResult:
        """
        将长文本切分为可翻译的 Chunk 列表。

        流程:
          1. 按 \n\n 切段落
          2. 贪心装箱 (Bin Packing)
          3. 超长段落按句子再切
          4. 注入上下文锚点
        """
        warnings: list[str] = []

        if not text or not text.strip():
            return ChunkResult()

        # Step 1: 按段落边界切分
        raw_paragraphs = [p.strip() for p in text.split("\n\n")]
        paragraphs = [p for p in raw_paragraphs if p]

        if not paragraphs:
            return ChunkResult()

        # Step 2: 贪心装箱
        chunks: list[Chunk] = []
        buffer: list[str] = []
        buffer_tokens = 0
        chunk_index = 0

        for para in paragraphs:
            para_tokens = count_tokens(para)

            # 情况 A: 单段超过硬上限 → 句子级切分
            if para_tokens > self.max:
                if buffer:
                    # 先保存当前缓冲区
                    chunks.append(Chunk(
                        index=chunk_index,
                        paragraphs=list(buffer),
                        token_count=buffer_tokens,
                    ))
                    chunk_index += 1
                    buffer = []
                    buffer_tokens = 0

                sub_paras = self._split_long_paragraph(para)
                for sp in sub_paras:
                    sp_tokens = count_tokens(sp)
                    chunks.append(Chunk(
                        index=chunk_index,
                        paragraphs=[sp],
                        token_count=sp_tokens,
                    ))
                    chunk_index += 1
                warnings.append(
                    f"Chunk {chunk_index - len(sub_paras)}-{chunk_index - 1}: "
                    f"长段落 ({para_tokens} tokens) 已拆为 {len(sub_paras)} 个子句"
                )
                continue

            # 情况 B: 加上当前段落后超出目标 → 保存缓冲区，开新 Chunk
            if buffer_tokens + para_tokens > self.target and buffer:
                chunks.append(Chunk(
                    index=chunk_index,
                    paragraphs=list(buffer),
                    token_count=buffer_tokens,
                ))
                chunk_index += 1
                buffer = []
                buffer_tokens = 0

            # 情况 C: 正常添加
            buffer.append(para)
            buffer_tokens += para_tokens

        # 最后一个 Chunk
        if buffer:
            chunks.append(Chunk(
                index=chunk_index,
                paragraphs=list(buffer),
                token_count=buffer_tokens,
            ))

        # Step 3: 注入上下文锚点（重叠段落）
        for i in range(1, len(chunks)):
            if self.overlap > 0 and chunks[i - 1].paragraphs:
                prev_last = chunks[i - 1].paragraphs[-1]
                # 只作为上下文发给 LLM，不参与该 Chunk 的输出
                chunks[i].overlap_prev = prev_last

        total_tokens = sum(c.token_count for c in chunks)

        result = ChunkResult(
            chunks=chunks,
            total_paragraphs=len(paragraphs),
            total_tokens=total_tokens,
            warnings=warnings,
        )

        logger.info(
            "分块完成: %d 段落 → %d chunks (共 %d tokens)",
            len(paragraphs), len(chunks), total_tokens,
        )
        for w in warnings:
            logger.debug("  ⚠ %s", w)

        return result

    def _split_long_paragraph(self, para: str) -> list[str]:
        """
        将超长段落按句子边界切分为可翻译的子段。

        策略:
          1. 先按句子边界切
          2. 再对子句做贪心装箱（合并到 target tokens 以内）
        """
        sentences = split_by_sentence(para)
        if len(sentences) <= 1:
            # 句子切分失败（无句号？）→ 按字符强制切分
            return self._force_split(para)

        sub_paras: list[str] = []
        buffer: list[str] = []
        buffer_tokens = 0

        for sent in sentences:
            sent_tokens = count_tokens(sent)
            if buffer_tokens + sent_tokens > self.target and buffer:
                sub_paras.append(" ".join(buffer))
                buffer = []
                buffer_tokens = 0
            buffer.append(sent)
            buffer_tokens += sent_tokens

        if buffer:
            sub_paras.append(" ".join(buffer))

        return sub_paras

    def _force_split(self, para: str) -> list[str]:
        """暴力切分：按字符数均分。"""
        chunk_chars = self.target * 3  # 约 3 字符/token
        parts = []
        for i in range(0, len(para), int(chunk_chars)):
            parts.append(para[i:i + int(chunk_chars)])
        return parts


# ── 便捷函数 ──


def chunk_text(
    text: str,
    target_tokens: int = 2000,
    max_tokens: int = 2400,
    overlap: int = 1,
) -> ChunkResult:
    """便捷函数：一行调分块。"""
    chunker = TextChunker(
        target_tokens=target_tokens,
        max_tokens=max_tokens,
        overlap=overlap,
    )
    return chunker.chunk(text)
