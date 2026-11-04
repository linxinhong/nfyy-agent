"""
出站脱敏模块
────────────────────────────────────────

拉取时调用远端 :9000 /desensitize 端点，把明文换成 token 化文本，
避免明文直接进入 LLM 上下文；写回前用本地映射把 token 还原成明文。

token 是不透明字符串（[TYPE_8hex] / [TYPE_16hex] 共存），原样传递、不解析。
"""

from __future__ import annotations

import re
from typing import Optional

import aiohttp

from .config import DESENSITIZE_TOKEN, DESENSITIZE_URL
from .utils import get_logger

logger = get_logger("desensitize")

# 超时 3~5s（脱敏端点内部 LLM 识别较慢，失败降级仅正则；此处取中值）
DESENSITIZE_TIMEOUT = 4.0

# token 形如 [PERSON_ab12cd34ef56ab12]，仅用于「未映射 token」告警检测
_TOKEN_RE = re.compile(r"\[[A-Z][A-Z0-9_]*_[0-9a-fA-F]{8,32}\]")

_session: Optional[aiohttp.ClientSession] = None


def _get_session() -> aiohttp.ClientSession:
    global _session
    if _session is None or _session.closed:
        _session = aiohttp.ClientSession()
    return _session


async def desensitize(text: str) -> dict:
    """POST /desensitize，返回 {"masked": bool, "masked_text": str, "entities": list}。

    任何异常/超时都降级为 {"masked": False, "masked_text": text}（明文 + 标记）。
    """
    if not text or not DESENSITIZE_URL:
        return {"masked": False, "masked_text": text, "entities": []}

    try:
        async with _get_session().post(
            DESENSITIZE_URL,
            json={"text": text},
            headers={"Authorization": f"Bearer {DESENSITIZE_TOKEN}"},
            timeout=aiohttp.ClientTimeout(total=DESENSITIZE_TIMEOUT),
        ) as resp:
            if resp.status != 200:
                logger.warning("脱敏端点返回 HTTP %s，降级明文", resp.status)
                return {"masked": False, "masked_text": text, "entities": []}
            data = await resp.json()
            if not isinstance(data, dict) or not isinstance(data.get("masked_text"), str):
                raise ValueError("invalid desensitize response")
            masked_text = data["masked_text"]
            entities = data.get("entities") or []
            masked = bool(data.get("masked", bool(entities)))
            return {"processed": True, "masked": masked, "masked_text": masked_text, "entities": entities}
    except Exception as e:
        logger.warning("脱敏请求失败（%s），降级明文", e)
        return {"masked": False, "masked_text": text, "entities": []}


def restore(text: str, mapping: dict[str, str]) -> str:
    """把文本中的 token 按 mapping（token → 明文）还原。未映射 token 原样保留。"""
    if not text or not mapping:
        return text
    for token, original in mapping.items():
        if token in text:
            text = text.replace(token, original)
    return text


def find_tokens(text: str) -> set[str]:
    """提取文本中所有形如 [TYPE_hex] 的 token（用于告警）。"""
    return set(_TOKEN_RE.findall(text or ""))


async def close_session() -> None:
    global _session
    if _session is not None and not _session.closed:
        await _session.close()
    _session = None
