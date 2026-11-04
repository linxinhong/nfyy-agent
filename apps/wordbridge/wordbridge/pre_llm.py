"""Pure helpers for attaching a frozen Word selection to an LLM turn."""
from __future__ import annotations

import re


_WORD_ACTION = re.compile(
    r"(?:选中|选取|这段|本文|文档|word|润色|改写|修改|优化|翻译|校对|扩写|缩写|总结|"
    r"rewrite|revise|edit|polish|translate|proofread|selection|document)",
    re.IGNORECASE,
)


def should_capture(user_message: object) -> bool:
    """Capture only turns that plausibly refer to Word content."""
    return isinstance(user_message, str) and bool(_WORD_ACTION.search(user_message))


def build_context(result: object) -> str:
    """Build ephemeral prompt context from a safe, gateway-processed result."""
    if not isinstance(result, dict) or result.get("status") != "ok":
        return ""
    regions = result.get("regions")
    if not isinstance(regions, list) or not regions:
        return ""
    region = regions[0]
    if not isinstance(region, dict):
        return ""
    region_id = region.get("region_id")
    text = region.get("text")
    if not isinstance(region_id, str) or not region_id or not isinstance(text, str) or not text:
        return ""
    return (
        "[WordBridge 已在本轮 LLM 请求前冻结用户的 Word 选区。\n"
        f"region_id: {region_id}\n"
        f"字符数: {region.get('char_count', len(text))}\n"
        "以下文本已经过企业网关检查/脱敏；格式快照和回填书签保留在本机。"
        "用户可继续移动光标或阅读文档。处理完成后使用同一 region_id 回填，"
        "不要重新获取当前选区。请在回复开头简短确认已读取该选区。\n"
        "--- 选区文本 ---\n"
        f"{text}\n"
        "--- 选区结束 ---]"
    )
