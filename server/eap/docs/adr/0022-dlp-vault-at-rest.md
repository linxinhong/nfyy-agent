# ADR-0022: DLP token vault at rest stays plaintext

Date: 2026-09-03 · Status: accepted · Task: TASK-031

## Context

The reversible-tokenization vault (`dlp_tokens.original_value`) concentrates
the very secrets DLP protects. Encrypting the column (AEAD with a KMS-held
key) is the textbook hardening.

## Decision

**Do not encrypt the vault column for V1.** The operator explicitly chose
availability over at-rest defense: with app-layer encryption, a lost key
makes every token permanently unrecoverable, and no key-management
infrastructure is deployed in this environment today. The database is
already access-controlled; reveal is governed, workspace-scoped and audited.

## Revisit triggers

- A KMS/key-management service becomes part of the deployment.
- The threat model adds "database dumps leave the perimeter" as in-scope.
- Compliance requirements mandate encryption at rest for pseudonym vaults.

Until then, compensating controls are: DB access control, audited reveal,
workspace scoping of tokens.
