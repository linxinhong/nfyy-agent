"""
翻译引擎 — 超长英文文本中英对照翻译

核心能力:
  - 滑窗分块 (chunker): Token + 段落边界双重感知
  - 多模型调用 (llm_client): kimi / deepseek / 硅基流动
  - 断点续传 (state_manager): SQLite 状态机
  - 双语组装 (assembler): 段落级 KV 对齐

公开 API:
  from wordbridge.translator import TranslatorEngine, resume, status

  engine = TranslatorEngine()
  result = await engine.translate(text, domain="legal")
  print(result.bilingual_text)

  # 断点续传
  result = await resume("task_id_xxx")

集成入口:
  from wordbridge.translator import TranslatorEngine
  → ws_server.py 中的 WordBridgeService 持有 TranslatorEngine 实例
  → 通过 MCP Tool translate_long_text 暴露给 Hermes
"""

from .engine import TranslatorEngine, TranslationResult, TranslationStatus
from .state_manager import TranslationStateManager

__all__ = [
    "TranslatorEngine",
    "TranslationResult",
    "TranslationStatus",
    "TranslationStateManager",
]
