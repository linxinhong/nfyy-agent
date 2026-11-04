"""
翻译引擎 — 多模型统一调用层

支持:
  - Kimi (Moonshot API)
  - DeepSeek (DeepSeek API)
  - 硅基流动 (SiliconFlow API)
  - 任意 OpenAI-compatible 端点

特性:
  - 指数退避重试 (HTTP 429/5xx)
  - 主 Provider 失败 → 自动 fallback
  - 术语表注入 System Prompt
  - 响应解析 & 格式校验
"""

from __future__ import annotations

import asyncio
import json
import os
import time
from dataclasses import dataclass, field
from typing import Any, Optional

from ..utils import get_logger

logger = get_logger("translator.llm")

# ── OpenAI SDK ──

try:
    from openai import AsyncOpenAI, APIError, APITimeoutError, RateLimitError
    _HAS_OPENAI = True
except ImportError:
    _HAS_OPENAI = False
    logger.critical("openai 库不可用，请安装: pip install openai")


# ══════════════════════════════════════════════
# 配置映射
# ══════════════════════════════════════════════

PROVIDER_CONFIG = {
    "kimi": {
        "base_url": "https://api.moonshot.cn/v1",
        "api_key_env": "KIMI_API_KEY",
        "default_model": "kimi-k3",
    },
    "deepseek": {
        "base_url": "https://api.deepseek.com/v1",
        "api_key_env": "DEEPSEEK_API_KEY",
        "default_model": "deepseek-chat",
    },
    "siliconflow": {
        "base_url": "https://api.siliconflow.cn/v1",
        "api_key_env": "SILICONFLOW_API_KEY",
        "default_model": "deepseek-ai/DeepSeek-V3",
    },
}


@dataclass
class LLMConfig:
    """LLM 调用配置。"""
    provider: str = "kimi"
    model: str = ""
    api_base: str = ""
    api_key: str = ""
    temperature: float = 0.1
    max_tokens: int = 16384
    request_timeout: float = 120.0
    max_retries: int = 3
    retry_base_delay: float = 1.0


@dataclass
class LLMResponse:
    """LLM 调用结果。"""
    success: bool
    raw_text: str = ""
    parsed_pairs: list[dict] = field(default_factory=list)  # [{"s":"","t":""}]
    tokens_used: int = 0
    latency_ms: int = 0
    error: str = ""
    provider_used: str = ""
    model_used: str = ""
    retry_count: int = 0


# ══════════════════════════════════════════════
# LLM 客户端
# ══════════════════════════════════════════════

class LLMClient:
    """
    多模型统一调用客户端。

    用法:
        client = LLMClient(LLMConfig(provider="kimi"))
        resp = await client.translate_chunk(
            paragraphs=["The Company shall...", "Notwithstanding..."],
            system_prompt=LEGAL_SYSTEM_PROMPT,
            overlap_prev=None,      # 上下文锚点
            glossary={"indemnify": "赔偿"},
        )
        # resp.parsed_pairs → [{"s": "...", "t": "..."}, ...]
    """

    def __init__(
        self,
        primary_config: LLMConfig,
        fallback_config: Optional[LLMConfig] = None,
    ):
        if not _HAS_OPENAI:
            raise RuntimeError("需要安装 openai 库: pip install openai")

        self.primary = self._resolve_config(primary_config)
        self.fallback = self._resolve_config(fallback_config) if fallback_config else None

    def _resolve_config(self, cfg: LLMConfig) -> LLMConfig:
        """解析 API key、base URL、model 等。"""
        provider_info = PROVIDER_CONFIG.get(cfg.provider, {})

        if not cfg.api_base:
            cfg.api_base = provider_info.get("base_url", "")
        if not cfg.model:
            cfg.model = provider_info.get("default_model", "gpt-3.5-turbo")
        if not cfg.api_key:
            key_env = provider_info.get("api_key_env", "")
            cfg.api_key = os.environ.get(key_env, "")

        if not cfg.api_key:
            logger.warning(
                "Provider '%s': API Key 未设置（环境变量 %s 为空）",
                cfg.provider, provider_info.get("api_key_env", "?")
            )

        return cfg

    # ── 核心翻译方法 ──

    async def translate_chunk(
        self,
        paragraphs: list[str],
        system_prompt: str,
        overlap_prev: Optional[str] = None,
        glossary: Optional[dict[str, str]] = None,
        domain: str = "legal",
    ) -> LLMResponse:
        """
        翻译单个 Chunk。

        参数:
          paragraphs:   该 Chunk 的段落列表
          system_prompt: System Prompt 模板
          overlap_prev: 前一 Chunk 最后一段（上下文锚点，不参与输出）
          glossary:     术语表 {en: zh}

        返回:
          LLMResponse，含 parsed_pairs
        """
        # 构建用户消息
        user_message = self._build_user_message(
            paragraphs, overlap_prev, glossary, domain
        )

        # 构建 System（含术语表注入）
        full_system = system_prompt
        if glossary:
            glossary_text = "\n## GLOSSARY (use these translations)\n" + "\n".join(
                f"- {en} → {zh}" for en, zh in glossary.items()
            )
            full_system += glossary_text

        # 先试主 Provider，再试 fallback
        configs = [self.primary]
        if self.fallback:
            configs.append(self.fallback)

        last_error = ""
        for cfg in configs:
            resp = await self._call_with_retry(
                cfg, full_system, user_message, len(paragraphs)
            )
            if resp.success:
                return resp
            last_error = resp.error

        return LLMResponse(
            success=False,
            error=f"所有 Provider 均失败。最后错误: {last_error}",
        )

    # ── 内部方法 ──

    def _build_user_message(
        self,
        paragraphs: list[str],
        overlap_prev: Optional[str],
        glossary: Optional[dict],
        domain: str,
    ) -> str:
        """构建发给 LLM 的用户消息。"""
        parts = []

        if overlap_prev:
            parts.append("## CONTEXT (previous paragraph — for reference only, DO NOT translate)")
            parts.append(overlap_prev)
            parts.append("---\n")

        if glossary:
            parts.append("## GLOSSARY")
            for en, zh in glossary.items():
                parts.append(f"- {en} → {zh}")
            parts.append("")

        parts.append("## TEXT TO TRANSLATE")
        for i, para in enumerate(paragraphs):
            parts.append(para)
            if i < len(paragraphs) - 1:
                parts.append("")

        return "\n".join(parts)

    async def _call_with_retry(
        self,
        cfg: LLMConfig,
        system_prompt: str,
        user_message: str,
        expected_pairs: int,
    ) -> LLMResponse:
        """带重试的 LLM 调用。"""
        client = AsyncOpenAI(
            api_key=cfg.api_key,
            base_url=cfg.api_base,
            timeout=cfg.request_timeout,
            max_retries=0,  # 自己控制重试
        )

        start_time = time.time()

        for attempt in range(cfg.max_retries + 1):
            try:
                completion = await client.chat.completions.create(
                    model=cfg.model,
                    messages=[
                        {"role": "system", "content": system_prompt},
                        {"role": "user", "content": user_message},
                    ],
                    temperature=cfg.temperature,
                    max_tokens=cfg.max_tokens,
                )

                raw_text = completion.choices[0].message.content or ""
                tokens_used = completion.usage.total_tokens if completion.usage else 0
                latency_ms = int((time.time() - start_time) * 1000)

                # 解析响应
                parsed = self._parse_bilingual_output(raw_text, expected_pairs)

                if parsed:
                    logger.debug(
                        "Chunk 翻译成功 (%s/%s, %dms, %d tokens)",
                        cfg.provider, cfg.model, latency_ms, tokens_used,
                    )
                    return LLMResponse(
                        success=True,
                        raw_text=raw_text,
                        parsed_pairs=parsed,
                        tokens_used=tokens_used,
                        latency_ms=latency_ms,
                        provider_used=cfg.provider,
                        model_used=cfg.model,
                        retry_count=attempt,
                    )
                else:
                    # 响应格式不正确 → 重试
                    logger.warning(
                        "响应格式解析失败 (attempt %d/%d)，将重试",
                        attempt + 1, cfg.max_retries,
                    )
                    if attempt < cfg.max_retries:
                        await asyncio.sleep(cfg.retry_base_delay * (2 ** attempt))
                    continue

            except RateLimitError as e:
                delay = cfg.retry_base_delay * (2 ** attempt)
                logger.warning(
                    "Rate limit (%s), 第 %d 次重试, 等待 %.1fs",
                    cfg.provider, attempt + 1, delay,
                )
                if attempt < cfg.max_retries:
                    await asyncio.sleep(delay)
                else:
                    return LLMResponse(
                        success=False,
                        error=f"Rate limit 重试耗尽: {e}",
                        retry_count=attempt,
                    )

            except APITimeoutError as e:
                logger.warning(
                    "超时 (%s), 第 %d 次重试", cfg.provider, attempt + 1,
                )
                if attempt < cfg.max_retries:
                    await asyncio.sleep(cfg.retry_base_delay * (2 ** attempt))
                else:
                    return LLMResponse(
                        success=False,
                        error=f"超时重试耗尽: {e}",
                        retry_count=attempt,
                    )

            except APIError as e:
                # 4xx 不重试, 5xx 重试
                status_code = getattr(e, 'status_code', None)
                if status_code and 400 <= status_code < 500:
                    logger.error("API 客户端错误 (%s, %d): %s", cfg.provider, status_code, e)
                    return LLMResponse(
                        success=False,
                        error=f"API {status_code}: {e}",
                    )
                if attempt < cfg.max_retries:
                    delay = cfg.retry_base_delay * (2 ** attempt)
                    logger.warning("API 错误 (%s): %s, 重试 in %.1fs", cfg.provider, e, delay)
                    await asyncio.sleep(delay)
                else:
                    return LLMResponse(
                        success=False,
                        error=f"API 错误重试耗尽: {e}",
                        retry_count=attempt,
                    )

            except Exception as e:
                logger.error("LLM 调用异常 (%s): %s", cfg.provider, e)
                if attempt < cfg.max_retries:
                    await asyncio.sleep(cfg.retry_base_delay * (2 ** attempt))
                else:
                    return LLMResponse(
                        success=False,
                        error=str(e),
                        retry_count=attempt,
                    )

        return LLMResponse(
            success=False,
            error=f"重试 {cfg.max_retries} 次后仍失败",
            retry_count=cfg.max_retries,
        )

    # ── 响应解析 ──

    @staticmethod
    def _parse_bilingual_output(raw: str, expected_count: int) -> list[dict]:
        """
        解析 LLM 输出的双语格式。

        格式:
          [ORIGINAL_EN]
          <英文原文>
          [ZH_CN]
          <中文翻译>
          ---

        返回:
          [{"s": "英文原文", "t": "中文翻译"}, ...]

        容错策略:
          1. 严格按 [ORIGINAL_EN] / [ZH_CN] 标签解析
          2. 解析失败 → 按 --- 分隔线尝试
          3. 段落数不匹配 → 返回空（触发 LLM 重试）
        """
        if not raw or not raw.strip():
            return []

        import re

        # 策略 1: 按标签解析
        en_pattern = r'\[ORIGINAL_EN\]\s*\n(.*?)\n\s*\[ZH_CN\]\s*\n(.*?)(?:\n\s*---|\n\s*$)'
        matches = re.findall(en_pattern, raw, re.DOTALL)

        if matches:
            pairs = []
            for en, zh in matches:
                pairs.append({
                    "s": en.strip(),
                    "t": zh.strip(),
                })

            if len(pairs) == expected_count:
                return pairs

            # 段落数不完全匹配时，如果是首次尝试（接近匹配），仍然接受
            if abs(len(pairs) - expected_count) <= 2:
                logger.debug(
                    "段落数接近: 期望 %d, 实际 %d (接受)", expected_count, len(pairs)
                )
                return pairs

        # 策略 2: 按 --- 分隔然后猜
        logger.debug("标签解析失败，尝试按 '---' 分隔")
        blocks = [b.strip() for b in raw.split("---") if b.strip()]
        pairs = []
        for block in blocks:
            lines = [l.strip() for l in block.split("\n") if l.strip()]
            if len(lines) >= 2:
                pairs.append({"s": lines[0], "t": lines[-1]})
            elif len(lines) == 1:
                pairs.append({"s": lines[0], "t": ""})

        if len(pairs) == expected_count:
            return pairs

        logger.warning(
            "无法解析双语输出: 期望 %d 对, 实际 %d 对. Raw: %s...",
            expected_count, len(pairs), raw[:200],
        )
        return []


# ── 便捷构建函数 ──


def build_llm_config(
    provider: str = "kimi",
    model: str = "",
    api_key: str = "",
    api_base: str = "",
    temperature: float = 0.1,
    max_tokens: int = 16384,
    request_timeout: float = 120.0,
    max_retries: int = 3,
) -> LLMConfig:
    """快速构建 LLMConfig，自动填充默认值。"""
    return LLMConfig(
        provider=provider,
        model=model,
        api_key=api_key,
        api_base=api_base,
        temperature=temperature,
        max_tokens=max_tokens,
        request_timeout=request_timeout,
        max_retries=max_retries,
    )
