"""
翻译引擎 — 双语组装器

从 SQLite 读取所有已完成 Chunk 的翻译结果，
按段落级 KV 对齐，拼接为最终的双语对照文本。

核心: 内存中维护 alignment 列表，确保 source↔translated 严格绑定。
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Optional

from ..utils import get_logger
from .state_manager import TranslationStateManager

logger = get_logger("translator.assembler")


# ══════════════════════════════════════════════
# 数据模型
# ══════════════════════════════════════════════

@dataclass
class AlignmentPair:
    """单个段落的中英对照。"""
    index: int                     # 段落序号（0-based）
    source: str                    # 英文原文
    translated: str                # 中文翻译
    chunk_index: int = 0           # 来自哪个 Chunk


@dataclass
class AssemblyResult:
    """组装结果。"""
    success: bool
    bilingual_text: str = ""       # 完整双语对照全文
    chinese_only: str = ""         # 仅中文
    english_only: str = ""         # 仅英文
    alignment: list[AlignmentPair] = field(default_factory=list)
    total_pairs: int = 0
    warnings: list[str] = field(default_factory=list)


# ══════════════════════════════════════════════
# 组装器
# ══════════════════════════════════════════════

class Assembler:
    """
    双语组装器。

    从 SQLite 读取翻译结果，验证段落数对齐，输出三种格式。
    """

    def __init__(self, state_manager: TranslationStateManager):
        self.state = state_manager

    def build(self, task_id: str) -> AssemblyResult:
        """
        从 DB 读取所有已完成 Chunk，组装最终输出。

        返回 AssemblyResult，含:
          - bilingual_text: 完整双语对照
          - chinese_only:   仅中文（用于写回 Word）
          - english_only:   仅英文（校对用）
          - alignment:      段落级 KV 绑定数组
        """
        task = self.state.get_task(task_id)
        if task is None:
            return AssemblyResult(success=False, warnings=[f"任务 {task_id} 不存在"])

        if task.status not in ("done", "partial_failed"):
            return AssemblyResult(
                success=False,
                warnings=[f"任务状态为 '{task.status}'，无法组装"],
            )

        # 读取所有 Chunk
        chunks = self.state.get_all_chunks(task_id)
        done_chunks = [c for c in chunks if c.status == "done"]

        if not done_chunks:
            return AssemblyResult(
                success=False,
                warnings=["没有已完成的 Chunk"],
            )

        # 解析每个 Chunk 的段落对
        alignment: list[AlignmentPair] = []
        warnings: list[str] = []
        pair_index = 0

        for chunk in done_chunks:
            try:
                source_paras = json.loads(chunk.source_json)
                translated_pairs = json.loads(chunk.translated_json) if chunk.translated_json else []
            except json.JSONDecodeError as e:
                warnings.append(f"Chunk {chunk.chunk_index} JSON 解析失败: {e}")
                continue

            if not translated_pairs:
                warnings.append(f"Chunk {chunk.chunk_index} 翻译结果为空")
                # 用空翻译填充
                for sp in source_paras:
                    alignment.append(AlignmentPair(
                        index=pair_index,
                        source=sp,
                        translated="[未翻译]",
                        chunk_index=chunk.chunk_index,
                    ))
                    pair_index += 1
                continue

            # 验证段落数
            src_count = len(source_paras)
            tgt_count = len(translated_pairs)

            if src_count != tgt_count:
                warnings.append(
                    f"Chunk {chunk.chunk_index}: "
                    f"段落数不匹配 (src={src_count}, tgt={tgt_count})"
                )
                # 尽可能对齐
                for i in range(min(src_count, tgt_count)):
                    alignment.append(AlignmentPair(
                        index=pair_index,
                        source=source_paras[i],
                        translated=translated_pairs[i].get("t", ""),
                        chunk_index=chunk.chunk_index,
                    ))
                    pair_index += 1
                # 多余的 source
                for i in range(tgt_count, src_count):
                    alignment.append(AlignmentPair(
                        index=pair_index,
                        source=source_paras[i],
                        translated="[翻译缺失]",
                        chunk_index=chunk.chunk_index,
                    ))
                    pair_index += 1
            else:
                for i, (sp, tp) in enumerate(zip(source_paras, translated_pairs)):
                    alignment.append(AlignmentPair(
                        index=pair_index,
                        source=sp,
                        translated=tp.get("t", ""),
                        chunk_index=chunk.chunk_index,
                    ))
                    pair_index += 1

        if not alignment:
            return AssemblyResult(success=False, warnings=warnings + ["对齐结果为空"])

        # 生成三种输出
        bilingual = self._render_bilingual(alignment)
        chinese_only = self._render_chinese_only(alignment)
        english_only = self._render_english_only(alignment)

        logger.info(
            "组装完成: %d 段落对, %d 个警告", len(alignment), len(warnings)
        )
        for w in warnings:
            logger.debug("  ⚠ %s", w)

        return AssemblyResult(
            success=True,
            bilingual_text=bilingual,
            chinese_only=chinese_only,
            english_only=english_only,
            alignment=alignment,
            total_pairs=len(alignment),
            warnings=warnings,
        )

    # ── 渲染方法 ──

    @staticmethod
    def _render_bilingual(alignment: list[AlignmentPair]) -> str:
        """生成完整双语对照。"""
        parts = []
        for pair in alignment:
            parts.append("[ORIGINAL_EN]")
            parts.append(pair.source)
            parts.append("")
            parts.append("[ZH_CN]")
            parts.append(pair.translated)
            parts.append("")
            parts.append("---")
            parts.append("")
        return "\n".join(parts).rstrip() + "\n"

    @staticmethod
    def _render_chinese_only(alignment: list[AlignmentPair]) -> str:
        """生成仅中文版本（用于写回 Word）。"""
        return "\n\n".join(pair.translated for pair in alignment)

    @staticmethod
    def _render_english_only(alignment: list[AlignmentPair]) -> str:
        """生成仅英文版本（校对用）。"""
        return "\n\n".join(pair.source for pair in alignment)


# ── 便捷函数 ──


def build_bilingual_output(
    state_manager: TranslationStateManager,
    task_id: str,
) -> dict:
    """便捷函数：一行调用组装。"""
    assembler = Assembler(state_manager)
    result = assembler.build(task_id)
    return {
        "success": result.success,
        "bilingual_text": result.bilingual_text,
        "chinese_only": result.chinese_only,
        "alignment": [
            {"index": a.index, "source": a.source, "translated": a.translated}
            for a in result.alignment
        ],
        "total_pairs": result.total_pairs,
        "warnings": result.warnings,
    }
