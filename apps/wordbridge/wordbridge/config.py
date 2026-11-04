"""
WordBridge 配置模块

从 config.yaml 加载配置，提供类型安全的配置访问。
"""

from __future__ import annotations

import os
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

import yaml

# 出站脱敏端点（PII 代理），拉取时 token 化、写回前本地映射还原（超时 3~5s）。
# 默认关闭：只有显式设置 DESENSITIZE_URL 时才会把 Word 文本发往该端点。
DESENSITIZE_URL = os.environ.get("DESENSITIZE_URL", "").strip()

# 脱敏端点鉴权（与网关 MCP 同一 tenant key；可用环境变量 GATEWAY_API_KEY 覆盖）
DESENSITIZE_TOKEN = os.environ.get("GATEWAY_API_KEY", "")  # 由宿主进程环境提供，仓库不含任何 key

# ── 默认配置（当 config.yaml 不存在时使用） ──
_DEFAULT_CONFIG_YAML = """
bridge:
  name: "WordBridge"
  version: "1.0.0"
  log_level: "INFO"
  log_file: "wordbridge.log"

server:
  host: "127.0.0.1"
  ws_port: 9876
  http_port: 9877
  max_message_size: 1048576

word:
  prog_id: "Word.Application"
  com_retry_max: 5
  com_retry_delay_ms: 200
  com_retry_backoff: 2.0
  health_check_interval: 5

hotkey:
  modifiers: ["Control", "Shift"]
  key: "H"

regions:
  database: "regions.db"
  ttl_seconds: 300
  conflict_similarity_threshold: 0.85

ooxml:
  strategy_order:
    - "node_replace"
    - "para_rebuild"
    - "plain_fallback"
  parse_timeout: 5
"""


@dataclass
class BridgeConfig:
    name: str = "WordBridge"
    version: str = "1.0.0"
    log_level: str = "INFO"
    log_file: str = "wordbridge.log"


@dataclass
class ServerConfig:
    host: str = "127.0.0.1"
    ws_port: int = 9876
    http_port: int = 9877
    max_message_size: int = 1_048_576


@dataclass
class WordConfig:
    prog_id: str = "Word.Application"
    com_retry_max: int = 5
    com_retry_delay_ms: int = 200
    com_retry_backoff: float = 2.0
    health_check_interval: int = 5
    selection_cache_enabled: bool = True
    selection_poll_seconds: float = 0.15
    selection_debounce_seconds: float = 0.3
    selection_cache_ttl_seconds: float = 300.0
    selection_cache_max_chars: int = 100_000


@dataclass
class HotkeyConfig:
    modifiers: list[str] = field(default_factory=lambda: ["Control", "Shift"])
    key: str = "H"


@dataclass
class RegionsConfig:
    database: str = "regions.db"
    ttl_seconds: int = 300
    conflict_similarity_threshold: float = 0.85


@dataclass
class OoxmlConfig:
    strategy_order: list[str] = field(
        default_factory=lambda: ["node_replace", "para_rebuild", "plain_fallback"]
    )
    parse_timeout: int = 5


@dataclass
class Config:
    bridge: BridgeConfig = field(default_factory=BridgeConfig)
    server: ServerConfig = field(default_factory=ServerConfig)
    word: WordConfig = field(default_factory=WordConfig)
    hotkey: HotkeyConfig = field(default_factory=HotkeyConfig)
    regions: RegionsConfig = field(default_factory=RegionsConfig)
    ooxml: OoxmlConfig = field(default_factory=OoxmlConfig)


def _dict_to_dataclass(cls, data: dict):
    """
    递归地将 dict 转换为 dataclass 实例。

    关键：使用 typing.get_type_hints() 而非 cls.__dataclass_fields__[key].type，
    因为 from __future__ import annotations 会使后者返回字符串而非实际类型。
    """
    import dataclasses
    import typing

    # 解析字符串注解为实际类型
    hints = typing.get_type_hints(cls)

    field_names = {f.name for f in dataclasses.fields(cls)}
    kwargs = {}
    for key, value in data.items():
        if key not in field_names:
            continue

        target_type = hints.get(key)
        if target_type is None:
            kwargs[key] = value
            continue

        # 剥掉 Optional[...] 外壳：Optional[X] = Union[X, None]
        origin = typing.get_origin(target_type)
        args = typing.get_args(target_type)
        if origin is typing.Union:
            non_none = [a for a in args if a is not type(None)]
            if len(non_none) == 1:
                target_type = non_none[0]

        if isinstance(value, dict) and dataclasses.is_dataclass(target_type):
            kwargs[key] = _dict_to_dataclass(target_type, value)
        else:
            kwargs[key] = value

    return cls(**kwargs)


def load_config(config_path: Optional[str] = None) -> Config:
    """
    加载配置，优先级：
    1. 传入的 config_path
    2. 可执行文件同目录下的 config.yaml
    3. 环境变量 WORDBRIDGE_CONFIG
    4. 默认配置
    """
    if config_path is None:
        # 可执行文件同目录
        if getattr(sys, 'frozen', False):
            app_dir = Path(sys.executable).parent
        else:
            app_dir = Path(__file__).resolve().parent.parent
        config_path = app_dir / "config.yaml"

        # 环境变量覆盖
        env_path = os.environ.get("WORDBRIDGE_CONFIG")
        if env_path:
            config_path = Path(env_path)

    config_path = Path(config_path)

    # 加载 YAML
    if config_path.exists():
        with open(config_path, 'r', encoding='utf-8') as f:
            raw = yaml.safe_load(f) or {}
    else:
        raw = yaml.safe_load(_DEFAULT_CONFIG_YAML) or {}

    # 转换为强类型配置
    cfg = _dict_to_dataclass(Config, raw)
    base = config_path.resolve().parent
    if not Path(cfg.regions.database).is_absolute():
        cfg.regions.database = str(base / cfg.regions.database)
    if cfg.bridge.log_file and not Path(cfg.bridge.log_file).is_absolute():
        cfg.bridge.log_file = str(base / cfg.bridge.log_file)
    return cfg


# 全局配置实例（在 main 中初始化）
_config: Optional[Config] = None


def get_config() -> Config:
    """获取全局配置实例。"""
    global _config
    if _config is None:
        _config = load_config()
    return _config


def set_config(cfg: Config) -> None:
    """设置全局配置实例。"""
    global _config
    _config = cfg
