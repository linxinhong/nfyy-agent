"""Bounded, local-only selection cache. Owned exclusively by the COM STA thread."""
from dataclasses import dataclass
from typing import Any


@dataclass
class SelectionSnapshot:
    doc_path: str
    story: int
    start: int
    end: int
    text: str
    text_hash: str
    range_ref: Any  # COM reference: never leave the STA thread

    @property
    def identity(self):
        return self.doc_path, self.story, self.start, self.end, self.text_hash


class SelectionCache:
    def __init__(self, debounce_seconds=0.3, ttl_seconds=300, max_chars=100_000):
        self.debounce = debounce_seconds
        self.ttl = ttl_seconds
        self.max_chars = max_chars
        self.clear()

    def clear(self):
        self._candidate = None
        self._ready = None
        self._since = 0.0
        self._last_seen = 0.0

    def observe(self, snapshot, now):
        if snapshot is None:
            # Keep only the last stable selection when the caret becomes empty.
            self._candidate = None
            self.get(now)
            return
        if len(snapshot.text) > self.max_chars:
            self.clear()
            return
        if self._candidate is None or self._candidate.identity != snapshot.identity:
            self._candidate = snapshot
            self._since = now
            if self._ready is not None and self._ready.identity != snapshot.identity:
                self._ready = None
        self._last_seen = now
        if now - self._since >= self.debounce:
            self._ready = snapshot

    def get(self, now):
        if now - self._last_seen > self.ttl:
            self.clear()
        return self._ready
