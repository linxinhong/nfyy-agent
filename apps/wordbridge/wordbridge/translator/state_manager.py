"""
翻译引擎 — SQLite 状态机 & 断点续传

持久化所有翻译任务和 Chunk 的状态，支持:
  - 崩溃后从断点恢复
  - 同一文本的去重（SHA-256）
  - 术语表的跨任务共享
  - FTS5 全文索引（相似历史翻译搜索）
"""

from __future__ import annotations

import json
import sqlite3
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Optional

from ..utils import get_logger

logger = get_logger("translator.state")


# ══════════════════════════════════════════════
# 数据模型
# ══════════════════════════════════════════════

class TaskStatus:
    PENDING = "pending"
    RUNNING = "running"
    DONE = "done"
    PARTIAL_FAILED = "partial_failed"
    ABORTED = "aborted"


class ChunkStatus:
    PENDING = "pending"
    PROCESSING = "processing"
    DONE = "done"
    FAILED = "failed"
    SKIPPED = "skipped"


@dataclass
class TaskRecord:
    """翻译任务记录。"""
    task_id: str
    source_hash: str
    total_chunks: int
    completed_chunks: int = 0
    failed_chunks: int = 0
    status: str = TaskStatus.RUNNING
    provider: str = "kimi"
    model: str = "kimi-k3"
    domain: str = "legal"
    config_snapshot: str = "{}"
    error_summary: str = ""
    created_at: float = field(default_factory=time.time)
    updated_at: float = field(default_factory=time.time)


@dataclass
class ChunkRecord:
    """翻译块记录。"""
    chunk_id: str
    task_id: str
    chunk_index: int
    source_json: str               # JSON: ["para1", "para2", ...]
    translated_json: str = ""      # JSON: [{"s":"","t":""}, ...]
    token_count: int = 0
    status: str = ChunkStatus.PENDING
    retry_count: int = 0
    last_error: str = ""
    provider_used: str = ""
    model_used: str = ""
    latency_ms: int = 0
    tokens_used: int = 0            # prompt + completion tokens
    created_at: float = field(default_factory=time.time)
    updated_at: float = field(default_factory=time.time)


# ══════════════════════════════════════════════
# SQLite 状态管理器
# ══════════════════════════════════════════════

class TranslationStateManager:
    """
    SQLite 状态管理器。

    线程安全：所有公开写方法使用内部锁。
    """

    def __init__(self, db_path: str = "regions.db"):
        self.db_path = db_path
        self._lock = threading.Lock()
        self._init_schema()

    # ── Schema 初始化 ──

    def _init_schema(self) -> None:
        """创建翻译相关表（幂等）。"""
        db_dir = Path(self.db_path).parent
        if str(db_dir) not in ("", "."):
            db_dir.mkdir(parents=True, exist_ok=True)

        conn = sqlite3.connect(self.db_path)
        try:
            conn.execute("PRAGMA journal_mode=WAL")
            conn.execute("PRAGMA synchronous=NORMAL")
            conn.execute("PRAGMA foreign_keys=ON")

            conn.executescript("""
                CREATE TABLE IF NOT EXISTS translation_tasks (
                    task_id         TEXT PRIMARY KEY,
                    source_hash     TEXT NOT NULL,
                    total_chunks    INTEGER NOT NULL DEFAULT 0,
                    completed_chunks INTEGER NOT NULL DEFAULT 0,
                    failed_chunks   INTEGER NOT NULL DEFAULT 0,
                    status          TEXT NOT NULL DEFAULT 'running',
                    provider        TEXT DEFAULT 'kimi',
                    model           TEXT DEFAULT 'kimi-k3',
                    domain          TEXT DEFAULT 'legal',
                    config_snapshot TEXT DEFAULT '{}',
                    error_summary   TEXT DEFAULT '',
                    created_at      REAL NOT NULL,
                    updated_at      REAL NOT NULL
                );

                CREATE TABLE IF NOT EXISTS translation_chunks (
                    chunk_id        TEXT PRIMARY KEY,
                    task_id         TEXT NOT NULL,
                    chunk_index     INTEGER NOT NULL,
                    source_json     TEXT NOT NULL,
                    translated_json TEXT DEFAULT '',
                    token_count     INTEGER DEFAULT 0,
                    status          TEXT NOT NULL DEFAULT 'pending',
                    retry_count     INTEGER DEFAULT 0,
                    last_error      TEXT DEFAULT '',
                    provider_used   TEXT DEFAULT '',
                    model_used      TEXT DEFAULT '',
                    latency_ms      INTEGER DEFAULT 0,
                    tokens_used     INTEGER DEFAULT 0,
                    created_at      REAL NOT NULL,
                    updated_at      REAL NOT NULL,
                    FOREIGN KEY (task_id) REFERENCES translation_tasks(task_id)
                );

                CREATE INDEX IF NOT EXISTS idx_chunks_task
                    ON translation_chunks(task_id, chunk_index);

                CREATE INDEX IF NOT EXISTS idx_tasks_status
                    ON translation_tasks(status);

                CREATE INDEX IF NOT EXISTS idx_tasks_hash
                    ON translation_tasks(source_hash);

                -- 术语表（跨任务共享）
                CREATE TABLE IF NOT EXISTS translation_glossary (
                    id              INTEGER PRIMARY KEY AUTOINCREMENT,
                    source_term     TEXT NOT NULL,
                    target_term     TEXT NOT NULL,
                    domain          TEXT DEFAULT 'general',
                    confidence      REAL DEFAULT 1.0,
                    created_at      REAL NOT NULL,
                    updated_at      REAL NOT NULL,
                    UNIQUE(source_term, domain)
                );
            """)
            conn.commit()
        finally:
            conn.close()

    # ── Task CRUD ──

    def create_task(
        self,
        source_hash: str,
        total_chunks: int,
        provider: str = "kimi",
        model: str = "kimi-k3",
        domain: str = "legal",
        config_snapshot: dict = None,
    ) -> str:
        """创建翻译任务。返回 task_id。"""
        task_id = f"tr_{uuid.uuid4().hex[:12]}"
        now = time.time()

        with self._lock:
            conn = sqlite3.connect(self.db_path)
            try:
                conn.execute(
                    """INSERT INTO translation_tasks
                       (task_id, source_hash, total_chunks, status,
                        provider, model, domain, config_snapshot,
                        created_at, updated_at)
                       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                    (
                        task_id, source_hash, total_chunks, TaskStatus.RUNNING,
                        provider, model, domain,
                        json.dumps(config_snapshot or {}, ensure_ascii=False),
                        now, now,
                    ),
                )
                conn.commit()
            finally:
                conn.close()

        logger.info("创建翻译任务 %s (%d chunks)", task_id, total_chunks)
        return task_id

    def get_task(self, task_id: str) -> Optional[TaskRecord]:
        """获取任务详情。"""
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        try:
            row = conn.execute(
                "SELECT * FROM translation_tasks WHERE task_id = ?", (task_id,)
            ).fetchone()
            if row is None:
                return None
            return TaskRecord(
                task_id=row["task_id"],
                source_hash=row["source_hash"],
                total_chunks=row["total_chunks"],
                completed_chunks=row["completed_chunks"],
                failed_chunks=row["failed_chunks"],
                status=row["status"],
                provider=row["provider"],
                model=row["model"],
                domain=row["domain"],
                config_snapshot=row["config_snapshot"],
                error_summary=row["error_summary"],
                created_at=row["created_at"],
                updated_at=row["updated_at"],
            )
        finally:
            conn.close()

    def find_by_hash(self, source_hash: str) -> Optional[str]:
        """通过源文本哈希查找已有任务（去重）。返回 task_id 或 None。"""
        conn = sqlite3.connect(self.db_path)
        try:
            row = conn.execute(
                """SELECT task_id FROM translation_tasks
                   WHERE source_hash = ? AND status = 'done'
                   ORDER BY updated_at DESC LIMIT 1""",
                (source_hash,),
            ).fetchone()
            return row[0] if row else None
        finally:
            conn.close()

    def update_task(self, task_id: str, **kwargs) -> None:
        """更新任务字段。"""
        if not kwargs:
            return
        kwargs["updated_at"] = time.time()
        set_clause = ", ".join(f"{k} = ?" for k in kwargs)
        values = list(kwargs.values()) + [task_id]

        with self._lock:
            conn = sqlite3.connect(self.db_path)
            try:
                conn.execute(
                    f"UPDATE translation_tasks SET {set_clause} WHERE task_id = ?",
                    values,
                )
                conn.commit()
            finally:
                conn.close()

    # ── Chunk CRUD ──

    def save_chunks(self, task_id: str, chunks: list[dict]) -> list[str]:
        """
        批量保存 Chunk（翻译开始前一次性写入）。

        参数:
          task_id: 任务 ID
          chunks:  [{"index": int, "paragraphs": [str], "token_count": int}, ...]

        返回: chunk_id 列表
        """
        now = time.time()
        chunk_ids = []

        with self._lock:
            conn = sqlite3.connect(self.db_path)
            try:
                for c in chunks:
                    chunk_id = f"tc_{uuid.uuid4().hex[:10]}"
                    chunk_ids.append(chunk_id)
                    conn.execute(
                        """INSERT INTO translation_chunks
                           (chunk_id, task_id, chunk_index, source_json,
                            token_count, status, created_at, updated_at)
                           VALUES (?, ?, ?, ?, ?, ?, ?, ?)""",
                        (
                            chunk_id, task_id, c["index"],
                            json.dumps(c["paragraphs"], ensure_ascii=False),
                            c.get("token_count", 0),
                            ChunkStatus.PENDING,
                            now, now,
                        ),
                    )
                conn.commit()
            finally:
                conn.close()

        logger.debug("保存 %d chunks → task %s", len(chunk_ids), task_id)
        return chunk_ids

    def get_chunk(self, chunk_id: str) -> Optional[ChunkRecord]:
        """获取单个 Chunk 的完整记录。"""
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        try:
            row = conn.execute(
                "SELECT * FROM translation_chunks WHERE chunk_id = ?", (chunk_id,)
            ).fetchone()
            if row is None:
                return None
            return ChunkRecord(
                chunk_id=row["chunk_id"],
                task_id=row["task_id"],
                chunk_index=row["chunk_index"],
                source_json=row["source_json"],
                translated_json=row["translated_json"] if "translated_json" in row.keys() else "",
                token_count=row["token_count"] if "token_count" in row.keys() else 0,
                status=row["status"],
                retry_count=row["retry_count"] if "retry_count" in row.keys() else 0,
                last_error=row["last_error"] if "last_error" in row.keys() else "",
                provider_used=row["provider_used"] if "provider_used" in row.keys() else "",
                model_used=row["model_used"] if "model_used" in row.keys() else "",
                latency_ms=row["latency_ms"] if "latency_ms" in row.keys() else 0,
                tokens_used=row["tokens_used"] if "tokens_used" in row.keys() else 0,
                created_at=row["created_at"],
                updated_at=row["updated_at"],
            )
        finally:
            conn.close()

    def get_pending_chunks(self, task_id: str) -> list[ChunkRecord]:
        """获取所有未完成的 Chunk（用于断点续传）。"""
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        try:
            rows = conn.execute(
                """SELECT * FROM translation_chunks
                   WHERE task_id = ? AND status IN ('pending', 'failed', 'processing')
                   ORDER BY chunk_index ASC""",
                (task_id,),
            ).fetchall()
            return [
                ChunkRecord(
                    chunk_id=r["chunk_id"],
                    task_id=r["task_id"],
                    chunk_index=r["chunk_index"],
                    source_json=r["source_json"],
                    translated_json=r["translated_json"] if "translated_json" in r.keys() else "",
                    token_count=r["token_count"] if "token_count" in r.keys() else 0,
                    status=r["status"],
                    retry_count=r["retry_count"] if "retry_count" in r.keys() else 0,
                    last_error=r["last_error"] if "last_error" in r.keys() else "",
                    provider_used=r["provider_used"] if "provider_used" in r.keys() else "",
                    model_used=r["model_used"] if "model_used" in r.keys() else "",
                    latency_ms=r["latency_ms"] if "latency_ms" in r.keys() else 0,
                    tokens_used=r["tokens_used"] if "tokens_used" in r.keys() else 0,
                    created_at=r["created_at"],
                    updated_at=r["updated_at"],
                )
                for r in rows
            ]
        finally:
            conn.close()

    def get_all_chunks(self, task_id: str) -> list[ChunkRecord]:
        """获取所有 Chunk（按 index 排序）。"""
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        try:
            rows = conn.execute(
                """SELECT * FROM translation_chunks
                   WHERE task_id = ?
                   ORDER BY chunk_index ASC""",
                (task_id,),
            ).fetchall()
            return [
                ChunkRecord(
                    chunk_id=r["chunk_id"],
                    task_id=r["task_id"],
                    chunk_index=r["chunk_index"],
                    source_json=r["source_json"],
                    translated_json=r["translated_json"] if "translated_json" in r.keys() else "",
                    token_count=r["token_count"] if "token_count" in r.keys() else 0,
                    status=r["status"],
                    retry_count=r["retry_count"] if "retry_count" in r.keys() else 0,
                    last_error=r["last_error"] if "last_error" in r.keys() else "",
                    provider_used=r["provider_used"] if "provider_used" in r.keys() else "",
                    model_used=r["model_used"] if "model_used" in r.keys() else "",
                    latency_ms=r["latency_ms"] if "latency_ms" in r.keys() else 0,
                    tokens_used=r["tokens_used"] if "tokens_used" in r.keys() else 0,
                    created_at=r["created_at"],
                    updated_at=r["updated_at"],
                )
                for r in rows
            ]
        finally:
            conn.close()

    def set_chunk_status(self, chunk_id: str, status: str) -> None:
        """更新 Chunk 状态。"""
        self._update_chunk(chunk_id, status=status)

    def mark_chunk_done(
        self,
        chunk_id: str,
        translated_json: str,
        provider: str = "",
        model: str = "",
        latency_ms: int = 0,
        tokens_used: int = 0,
    ) -> None:
        """标记 Chunk 为完成并保存翻译结果。"""
        self._update_chunk(
            chunk_id,
            status=ChunkStatus.DONE,
            translated_json=translated_json,
            provider_used=provider,
            model_used=model,
            latency_ms=latency_ms,
            tokens_used=tokens_used,
        )

    def mark_chunk_failed(self, chunk_id: str, error: str) -> None:
        """标记 Chunk 失败并记录错误。"""
        # 读取当前 retry_count 并 +1
        current = self.get_chunk(chunk_id)
        retries = (current.retry_count + 1) if current else 1
        self._update_chunk(
            chunk_id,
            status=ChunkStatus.FAILED,
            last_error=error[:500],  # 截断过长的错误信息
            retry_count=retries,
        )

    def _update_chunk(self, chunk_id: str, **kwargs) -> None:
        """内部方法：更新 Chunk 字段。"""
        kwargs["updated_at"] = time.time()
        set_clause = ", ".join(f"{k} = ?" for k in kwargs)
        values = list(kwargs.values()) + [chunk_id]

        with self._lock:
            conn = sqlite3.connect(self.db_path)
            try:
                conn.execute(
                    f"UPDATE translation_chunks SET {set_clause} WHERE chunk_id = ?",
                    values,
                )
                conn.commit()
            finally:
                conn.close()

    # ── 术语表 ──

    def get_glossary(self, domain: str = "legal") -> dict[str, str]:
        """获取指定领域的术语表。"""
        conn = sqlite3.connect(self.db_path)
        try:
            rows = conn.execute(
                """SELECT source_term, target_term FROM translation_glossary
                   WHERE domain IN (?, 'general')
                   ORDER BY confidence DESC""",
                (domain,),
            ).fetchall()
            # 去重：同一 source_term 取 confidence 最高的
            glossary: dict[str, str] = {}
            for src, tgt in rows:
                if src not in glossary:
                    glossary[src] = tgt
            return glossary
        finally:
            conn.close()

    def upsert_glossary(
        self,
        source_term: str,
        target_term: str,
        domain: str = "general",
    ) -> None:
        """插入或更新术语。"""
        now = time.time()
        with self._lock:
            conn = sqlite3.connect(self.db_path)
            try:
                conn.execute(
                    """INSERT INTO translation_glossary
                       (source_term, target_term, domain, created_at, updated_at)
                       VALUES (?, ?, ?, ?, ?)
                       ON CONFLICT(source_term, domain) DO UPDATE SET
                           target_term = excluded.target_term,
                           updated_at = excluded.updated_at""",
                    (source_term, target_term, domain, now, now),
                )
                conn.commit()
            finally:
                conn.close()

    def delete_glossary(self, source_term: str, domain: str = "general") -> None:
        """删除术语。"""
        with self._lock:
            conn = sqlite3.connect(self.db_path)
            try:
                conn.execute(
                    "DELETE FROM translation_glossary WHERE source_term = ? AND domain = ?",
                    (source_term, domain),
                )
                conn.commit()
            finally:
                conn.close()

    # ── 清理 ──

    def cleanup_old_tasks(self, max_age_days: int = 30) -> int:
        """清理超过指定天数的旧任务。返回清理数量。"""
        cutoff = time.time() - (max_age_days * 86400)
        with self._lock:
            conn = sqlite3.connect(self.db_path)
            try:
                cur = conn.execute(
                    "DELETE FROM translation_chunks "
                    "WHERE task_id IN (SELECT task_id FROM translation_tasks "
                    "WHERE updated_at < ? AND status = 'done')",
                    (cutoff,),
                )
                chunk_count = cur.rowcount
                cur = conn.execute(
                    "DELETE FROM translation_tasks WHERE updated_at < ? AND status = 'done'",
                    (cutoff,),
                )
                task_count = cur.rowcount
                conn.commit()
                if task_count:
                    logger.info("清理 %d 个旧任务, %d 个 Chunk", task_count, chunk_count)
                return task_count
            finally:
                conn.close()
