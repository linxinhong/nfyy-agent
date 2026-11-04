"""
WordBridge 工具模块
- 日志管理
- 辅助函数
"""

from __future__ import annotations

import hashlib
import logging
import logging.handlers
import sys
from pathlib import Path
from typing import Optional

# ── 全局 Logger ──
_logger: Optional[logging.Logger] = None


def setup_logging(
    log_level: str = "INFO",
    log_file: Optional[str] = None,
    name: str = "wordbridge",
    use_stderr: bool = False,
    console: bool = True,
) -> logging.Logger:
    """
    初始化日志系统。
      - 控制台输出（带颜色，如果安装了 colorlog）
      - 文件输出（如果指定了 log_file）
      - use_stderr=True 时控制台输出到 stderr（stdio MCP 模式）
    """
    global _logger

    logger = logging.getLogger(name)
    logger.setLevel(getattr(logging, log_level.upper(), logging.INFO))

    while logger.handlers:
        logger.removeHandler(logger.handlers[0])

    fmt = "%(asctime)s.%(msecs)03d | %(levelname)-7s | %(name)s | %(message)s"
    datefmt = "%H:%M:%S"
    
    log_stream = sys.stderr if use_stderr else sys.stdout

    # stdio MCP 模式可禁用控制台日志，保证协议管道里只有 JSON-RPC。
    if console:
        try:
            import colorlog
            console_handler = colorlog.StreamHandler(log_stream)
            console_handler.setFormatter(colorlog.ColoredFormatter(
                f"%(log_color)s{fmt}%(reset)s",
                datefmt=datefmt,
                log_colors={
                    'DEBUG': 'cyan',
                    'INFO': 'green',
                    'WARNING': 'yellow',
                    'ERROR': 'red',
                    'CRITICAL': 'red,bg_white',
                }
            ))
        except ImportError:
            console_handler = logging.StreamHandler(log_stream)
            console_handler.setFormatter(logging.Formatter(fmt, datefmt=datefmt))
        logger.addHandler(console_handler)

    # 文件 handler
    if log_file:
        log_path = Path(log_file)
        log_path.parent.mkdir(parents=True, exist_ok=True)
        file_handler = logging.handlers.RotatingFileHandler(
            log_path, maxBytes=5 * 1024 * 1024, backupCount=2, encoding='utf-8'
        )
        file_handler.setFormatter(logging.Formatter(
            "%(asctime)s | %(levelname)-7s | %(name)s | %(message)s"
        ))
        logger.addHandler(file_handler)

    _logger = logger
    return logger


def get_logger(name: Optional[str] = None) -> logging.Logger:
    """获取 logger 实例。"""
    if name:
        return logging.getLogger(f"wordbridge.{name}")
    return _logger or logging.getLogger("wordbridge")


# ── 辅助函数 ──

def text_hash(text: str) -> str:
    """计算文本的 SHA-256 指纹（截取前 16 位 hex）。"""
    return hashlib.sha256(text.encode('utf-8', errors='replace')).hexdigest()[:16]


def truncate(text: str, max_len: int = 100) -> str:
    """截断文本用于日志/预览显示。"""
    if len(text) <= max_len:
        return text
    return text[:max_len] + "..."
