# ADR-0010: EAP CLI is a V1 control bridge

- Status: Accepted
- Date: 2026-09-01

## Context

EAP already defines one public Gateway, a canonical Principal, Workspace-scoped authorization, bootstrap discovery, Skill Registry, and Agent distribution concepts. Without an official local client, Skill lifecycle and client setup would depend on Portal clicks and manual file copying, leaving no reliable way to reproduce or revoke a Workspace Agent environment.

The inference namespace `/v1/*` is intentionally compatible with OpenAI and Anthropic clients. It cannot also become the EAP package-management control plane.

## Decision

Ship a Go CLI named `eap` in V1. It is both the EAP developer tool and the local Agent bootstrap client.

```text
Human          -> EAP Portal
Agent / CI     -> EAP Gateway + agk_*
Local machine  -> EAP CLI + device credential (or agk_* for headless use)
```

All three paths use `https://agent.company.com`, the canonical Principal, Workspace membership, roles, scopes, Policy, DLP, Trace, and Audit. Device Authorization does not create a second identity system: it issues a revocable, rotating, device-bound CLI credential that terminates at the Agent Gateway. Its refresh material is stored only in the OS keychain. CI and servers use `EAP_BASE_URL` plus `EAP_API_KEY=agk_*`.

Control-plane APIs remain under `/enterprise/v1/*`; `/v1/*` remains inference-only.

## Skill supply chain

A Skill contains standard `SKILL.md` plus enterprise metadata in `skill.yaml`. The CLI creates a deterministic `tar.gz`, validates structure, scans secrets/scripts/dependencies, and submits its SHA-256 digest. The server repeats every security validation and treats CLI results only as developer feedback.

`name@version` is immutable. Versions move through `pending-review`, `approved`, `released`, `deprecated`, or `revoked`. Release signing happens server-side after approval with a dedicated package-release key backed by the configured key-management boundary. Package keys are distinct from internal JWT keys and are published through `/.well-known/eap-package-keys.json`. Installers verify both digest and release signature before atomic extraction.

Downloaded packages never execute scripts automatically. `eap skill test` may execute the current developer checkout's explicitly declared test command only when the user invokes it.

## Workspace synchronization

`.eap/workspace.yaml` is the human-authored desired state. `eap.lock` is generated resolution data containing exact versions, digests, signing key IDs, Workspace generation, and selected Agent targets.

`eap sync` resolves server policy and desired state, presents a plan, then applies atomic changes. It may upgrade or remove only files recorded as EAP-managed. Unmanaged local content is preserved. Interrupted application rolls back to the previous managed state.

The server distributes standard packages and policy, not client-specific filesystem layouts. CLI adapters own detection and installation for Harness, Codex, Claude Code, Pi, and project-local `.agents/skills`. Harness and Codex are the V1 qualification pair; other adapters must pass their contract tests but do not block the final compatibility gate on unavailable external binaries.

## Agent distribution

Agent releases are versioned by client, OS, and architecture and carry minimum-version policy, digest, and platform signature. Downloads use a short-lived ticket redeemed through the EAP domain; internal object-store locations are never returned. CLI installation verifies release metadata and performs an atomic update with rollback.

## Consequences

- Device Login, Skill Registry, client auto-configuration, and Agent version distribution move into V1.
- Artifact CLI, Knowledge Candidate, general Agent Runtime, workflow building, and broad client grey release remain outside this decision.
- Portal owns discovery, review, permissions, and operations. CLI owns local creation, packaging, publication, installation, synchronization, diagnostics, and client adaptation.
