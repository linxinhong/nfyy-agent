"""
Region Registry 模块
────────────────────────────────────────

管理所有已捕获的选区（Region）的完整生命周期。

设计要点：
  - 内存 + SQLite 双存储（内存为热数据，SQLite 为持久化 + 崩溃恢复）
  - 状态机确保 Region 生命周期可追踪
  - 超时自动清理
  - 线程安全（内部使用锁保护共享状态）

状态机：
  captured → processing → ready ──→ applying → applied
                           │                    ↑
                           ├──→ conflict ───────┤
                           ├──→ rejected ────────┤
                           └──→ expired ←────────┘
"""

from __future__ import annotations

import json
import sqlite3
import threading
import time
import uuid
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Any, Optional

from .config import RegionsConfig, get_config
from .utils import get_logger, text_hash

logger = get_logger("registry")


# ══════════════════════════════════════════════
# 数据模型
# ══════════════════════════════════════════════

class RegionStatus(str, Enum):
    """Region 生命周期状态。"""
    CAPTURED = "captured"        # 已捕获，等待 Hermes 处理
    PROCESSING = "processing"    # Hermes 正在处理
    READY = "ready"              # AI 结果就绪，等待应用
    APPLYING = "applying"        # 正在应用修改
    APPLIED = "applied"          # 已成功应用
    CONFLICT = "conflict"        # 检测到冲突，等待用户决策
    REJECTED = "rejected"        # 用户拒绝修改
    EXPIRED = "expired"          # 超时失效


@dataclass
class Region:
    """单个选区 Region 的完整快照。"""
    region_id: str
    text: str                    # 原始纯文本
    text_hash: str               # 文本指纹（SHA-256 前 16 位）
    ooxml: str                   # 原始 OOXML
    bookmark: str                # Word 书签名
    doc_path: str                # 源文档路径
    status: RegionStatus = RegionStatus.CAPTURED
    captured_at: float = field(default_factory=time.time)
    processed_at: Optional[float] = None
    applied_at: Optional[float] = None
    ai_text: Optional[str] = None        # AI 处理后的新文本
    ai_ooxml: Optional[str] = None        # 构建好的 OOXML
    current_hash: Optional[str] = None     # 应用前的最新 hash（用于冲突检测）
    conflict_text: Optional[str] = None    # 冲突时的当前文本
    user_instruction: Optional[str] = None  # 用户给 Hermes 的指令
    version: int = 1
    metadata: dict = field(default_factory=dict)


# ══════════════════════════════════════════════
# Region Registry
# ══════════════════════════════════════════════

class RegionRegistry:
    """
    Region 注册表。

    线程安全：所有公开方法使用内部锁保护。
    """

    def __init__(
        self,
        db_path: Optional[str] = None,
        config: Optional[RegionsConfig] = None,
    ):
        self.config = config or get_config().regions
        self.db_path = db_path or self.config.database
        self.ttl = self.config.ttl_seconds
        self._lock = threading.RLock()
        self._regions: dict[str, Region] = {}
        self._init_db()

    # ── CRUD ──

    def create(self, capture_data: dict) -> Region:
        """从捕获数据创建 Region。"""
        region = Region(
            region_id=capture_data["region_id"],
            text=capture_data["text"],
            text_hash=capture_data["hash"],
            ooxml=capture_data.get("ooxml", ""),
            bookmark=capture_data["bookmark"],
            doc_path=capture_data.get("doc_path", ""),
        )
        with self._lock:
            self._regions[region.region_id] = region
            self._persist(region)
        logger.info("Region %s 已创建 (%d chars)", region.region_id, len(region.text))
        return region

    def get(self, region_id: str) -> Optional[Region]:
        """获取 Region。"""
        with self._lock:
            region = self._regions.get(region_id)
            if region is None:
                # A pre-LLM capture can be created by the short-lived capture
                # helper while Hermes' long-lived MCP process is already up.
                # Refresh from SQLite on a miss so both processes share the
                # same durable Region without restarting the MCP server.
                self.recover_from_db()
                region = self._regions.get(region_id)
            return region

    def update(self, region_id: str, **kwargs) -> Optional[Region]:
        """
        更新 Region 字段。
        自动递增 version。
        """
        with self._lock:
            region = self._regions.get(region_id)
            if region is None:
                return None
            for key, value in kwargs.items():
                if hasattr(region, key):
                    setattr(region, key, value)
            region.version += 1
            self._persist(region)
            return region

    def transition(self, region_id: str, new_status: RegionStatus) -> Optional[Region]:
        """原子状态转移。"""
        with self._lock:
            region = self._regions.get(region_id)
            if region is None:
                return None
            old_status = region.status
            region.status = new_status
            region.version += 1
            self._persist(region)
            logger.debug("Region %s: %s → %s", region_id, old_status, new_status)
            return region

    def delete(self, region_id: str) -> bool:
        """删除 Region。"""
        with self._lock:
            if region_id in self._regions:
                del self._regions[region_id]
                self._delete_from_db(region_id)
                logger.debug("Region %s 已删除", region_id)
                return True
            return False

    # ── 批量查询 ──

    def list_active(self) -> list[Region]:
        """列出所有活跃的 Region（非终态）。"""
        terminal = {RegionStatus.APPLIED, RegionStatus.REJECTED, RegionStatus.EXPIRED}
        with self._lock:
            return [
                r for r in self._regions.values()
                if r.status not in terminal
            ]

    def list_by_doc(self, doc_path: str) -> list[Region]:
        """列出指定文档的所有 Region。"""
        with self._lock:
            return [
                r for r in self._regions.values()
                if r.doc_path == doc_path
            ]

    def list_by_status(self, status: RegionStatus) -> list[Region]:
        """按状态列出 Region。"""
        with self._lock:
            return [
                r for r in self._regions.values()
                if r.status == status
            ]

    # ── 冲突检测 ──

    def check_conflict(self, region_id: str, current_hash: str) -> dict:
        """
        检查 Region 的当前文本哈希是否与捕获时一致。

        返回：
          {
            "status": "match" | "conflict" | "minor_diff" | "not_found",
            "original_hash": str,
            "current_hash": str,
            "similarity": float | None,
          }
        """
        region = self.get(region_id)
        if region is None:
            return {"status": "not_found"}

        if current_hash == region.text_hash:
            return {
                "status": "match",
                "original_hash": region.text_hash,
                "current_hash": current_hash,
                "similarity": 1.0,
            }

        # 哈希不匹配，用相似度判断差异程度
        # 注意：这里拿不到当前文本，相似度检查在 ws_server 层做
        return {
            "status": "conflict",
            "original_hash": region.text_hash,
            "current_hash": current_hash,
            "similarity": None,  # 需要调用方传入当前文本来计算
        }

    def compute_similarity(self, text_a: str, text_b: str) -> float:
        """
        计算两段文本的 Jaccard 相似度（以词为单位）。
        用于判断用户的修改是大改动还是小改动。
        """
        if not text_a and not text_b:
            return 1.0
        if not text_a or not text_b:
            return 0.0
        set_a = set(text_a.split())
        set_b = set(text_b.split())
        intersection = set_a & set_b
        union = set_a | set_b
        if not union:
            return 0.0
        return len(intersection) / len(union)

    # ── 超时清理 ──

    def cleanup_expired(self) -> int:
        """
        清理超过 TTL 的 Region。
        返回清理数量。
        """
        now = time.time()
        expired_ids = []
        with self._lock:
            for rid, region in self._regions.items():
                if region.status in (RegionStatus.APPLIED, RegionStatus.REJECTED):
                    # 已终态超过 TTL
                    if region.applied_at and (now - region.applied_at) > self.ttl:
                        expired_ids.append(rid)
                elif region.status == RegionStatus.EXPIRED:
                    expired_ids.append(rid)
                elif (now - region.captured_at) > self.ttl:
                    expired_ids.append(rid)

            for rid in expired_ids:
                del self._regions[rid]
                self._delete_from_db(rid)

        if expired_ids:
            logger.info("清理 %d 个过期 Region", len(expired_ids))
        return len(expired_ids)

    def count(self) -> int:
        """当前 Region 总数。"""
        with self._lock:
            return len(self._regions)

    # ── 崩溃恢复 ──

    def recover_from_db(self) -> int:
        """
        从 SQLite 恢复中断的 Region。
        在 Bridge 启动时调用。

        返回恢复的 Region 数量。
        """
        try:
            with self._lock:
                conn = sqlite3.connect(self.db_path)
                conn.row_factory = sqlite3.Row
                rows = conn.execute(
                    "SELECT * FROM regions WHERE status NOT IN ('applied', 'rejected', 'expired')"
                ).fetchall()
                conn.close()

                recovered = 0
                for row in rows:
                    row = dict(row)
                    rid = row["region_id"]
                    if rid not in self._regions:
                        try:
                            region = Region(
                                region_id=rid,
                                text=row["text"],
                                text_hash=row["text_hash"],
                                ooxml=row.get("ooxml", ""),
                                bookmark=row.get("bookmark", rid),
                                doc_path=row.get("doc_path", ""),
                                status=RegionStatus(row["status"]),
                                captured_at=float(row["captured_at"]),
                                ai_text=row.get("ai_text"),
                                ai_ooxml=row.get("ai_ooxml"),
                                processed_at=row.get("processed_at"),
                                applied_at=row.get("applied_at"),
                                current_hash=row.get("current_hash"),
                                conflict_text=row.get("conflict_text"),
                                user_instruction=row.get("user_instruction"),
                                version=row.get("version", 1),
                                metadata=json.loads(row.get("metadata") or "{}"),
                            )
                            self._regions[rid] = region
                            recovered += 1
                        except Exception as e:
                            logger.warning("恢复 Region %s 失败: %s", rid, e)

                if recovered:
                    logger.info("从数据库恢复了 %d 个 Region", recovered)
                return recovered

        except Exception as e:
            logger.error("数据库恢复失败: %s", e)
            return 0

    # ── 内部方法 ──

    def _init_db(self) -> None:
        """初始化 SQLite 数据库。"""
        db_dir = Path(self.db_path).parent
        if str(db_dir) not in ("", "."):
            db_dir.mkdir(parents=True, exist_ok=True)

        conn = sqlite3.connect(self.db_path)
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA synchronous=NORMAL")
        conn.execute("""
            CREATE TABLE IF NOT EXISTS regions (
                region_id    TEXT PRIMARY KEY,
                text         TEXT NOT NULL,
                text_hash    TEXT NOT NULL,
                ooxml        TEXT,
                bookmark     TEXT NOT NULL,
                doc_path     TEXT,
                status       TEXT NOT NULL DEFAULT 'captured',
                captured_at  REAL NOT NULL,
                processed_at REAL,
                applied_at   REAL,
                ai_text      TEXT,
                ai_ooxml     TEXT,
                current_hash TEXT,
                conflict_text TEXT,
                user_instruction TEXT,
                version      INTEGER DEFAULT 1,
                metadata     TEXT DEFAULT '{}'
            )
        """)
        conn.commit()
        conn.close()
        logger.debug("SQLite 数据库已初始化: %s", self.db_path)

    def _persist(self, region: Region) -> None:
        """将 Region 持久化到 SQLite（UPSERT）。"""
        try:
            conn = sqlite3.connect(self.db_path)
            conn.execute("""
                INSERT OR REPLACE INTO regions (
                    region_id, text, text_hash, ooxml, bookmark, doc_path,
                    status, captured_at, processed_at, applied_at,
                    ai_text, ai_ooxml, current_hash, conflict_text,
                    user_instruction, version, metadata
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """, (
                region.region_id,
                region.text,
                region.text_hash,
                region.ooxml,
                region.bookmark,
                region.doc_path,
                region.status.value,
                region.captured_at,
                region.processed_at,
                region.applied_at,
                region.ai_text,
                region.ai_ooxml,
                region.current_hash,
                region.conflict_text,
                region.user_instruction,
                region.version,
                json.dumps(region.metadata, ensure_ascii=False),
            ))
            conn.commit()
            conn.close()
        except Exception as e:
            logger.error("持久化 Region %s 失败: %s", region.region_id, e)
            # 持久化失败不中断业务 — 数据在内存中仍然可用

    def _delete_from_db(self, region_id: str) -> None:
        """从 SQLite 删除 Region。"""
        try:
            conn = sqlite3.connect(self.db_path)
            conn.execute("DELETE FROM regions WHERE region_id = ?", (region_id,))
            conn.commit()
            conn.close()
        except Exception as e:
            logger.warning("从数据库删除 Region %s 失败: %s", region_id, e)
