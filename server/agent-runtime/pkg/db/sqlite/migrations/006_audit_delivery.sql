-- Migration 2026080905 (reserved): audit delivery state. Forward-only.
ALTER TABLE outbox_events ADD COLUMN delivered_at TEXT;
