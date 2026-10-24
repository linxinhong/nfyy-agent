SHELL := /bin/bash
GO ?= go
COMPOSE := docker compose -f deploy/docker-compose.yml
GOPROXY ?= $(shell go env GOPROXY)

.DEFAULT_GOAL := verify-foundation

.PHONY: verify-foundation build vet fmt-check test race \
        verify-identity-slice integration-identity \
        verify-llm-kernel-spike test-llm-kernel diag-upstream-race \
        verify-mcp-slice integration-mcp \
        verify-provider-governance integration-provider-governance \
        verify-production-oidc integration-production-oidc \
        verify-policy-dlp-audit integration-policy-dlp-audit \
        verify-mcp-governance integration-mcp-governance \
        verify-knowledge-slice integration-knowledge-slice \
        verify-portal-shell portal-check portal-e2e \
        verify-portal-deploy portal-deploy-e2e portal-deploy-compose \
        verify-m0-m3 integration-m0m3 \
        run-gateway dev-infra dev-up dev-ready dev-down dev-nuke

## verify-foundation: repository verification target for the foundation slice.
## Real gates only: formatting, vet, build, unit/contract/security tests, race.
verify-foundation: fmt-check vet build test race
	@echo "verify-foundation: PASS"

## verify-llm-kernel-spike: TASK-002 acceptance target. Foundation gates plus
## the Bifrost kernel integration slice (security gates, fallback, streaming
## cancellation, error normalization, usage hook).
verify-llm-kernel-spike: verify-foundation test-llm-kernel
	@echo "verify-llm-kernel-spike: PASS"

test-llm-kernel:
	$(GO) test ./packages/llmkernel/ -count=1 -v

## diag-upstream-race: diagnostic only, NOT a gate. Reproduces the bifrost
## v1.8.4 stream-cancellation data race (ADR-0004); re-run when upgrading the
## pin to confirm the upstream fix, then fold the test back into `race`.
diag-upstream-race:
	$(GO) test -race -tags upstreamrace ./packages/llmkernel/ -run TestStreamChatCancellation -count=1

## verify-mcp-slice: TASK-004 acceptance target. Identity gates plus the
## governed MCP slice: discovery/execution workspace isolation, credential
## references, and audit evidence against real PostgreSQL and Redis.
verify-mcp-slice: verify-identity-slice integration-mcp
	@echo "verify-mcp-slice: PASS"

integration-mcp:
	$(COMPOSE) up -d postgres redis
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestMCPSliceIntegration' -count=1 -v

## verify-m0-m3: TASK-006 acceptance target — the full M0-M3 chain plus the
## cross-slice journey (login -> key -> LLM protocol -> MCP read tool, with
## fail-closed revocation/DLP/policy/cross-workspace evidence).
verify-m0-m3: verify-mcp-slice integration-m0m3 verify-portal-deploy
	@echo "verify-m0-m3: PASS"

integration-m0m3:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestM0M3JourneyIntegration' -count=1 -v

## verify-provider-governance: TASK-008 acceptance target. MCP-slice gates
## plus the routed dual-provider journey: deterministic failover, durable
## idempotent usage/cost ledger, credential-failure and unknown-pricing
## behavior against real PostgreSQL and Redis.
verify-provider-governance: verify-mcp-slice integration-provider-governance
	@echo "verify-provider-governance: PASS"

integration-provider-governance:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestProviderGovernanceIntegration' -count=1 -v

## verify-production-oidc: TASK-009 acceptance target. Identity gates plus
## the IdP-stub browser flow (state/nonce/PKCE, secure session, logout) and
## fail-closed forgery/replay/expiry/removal denials with audit evidence.
verify-production-oidc: verify-mcp-slice integration-production-oidc
	@echo "verify-production-oidc: PASS"

integration-production-oidc:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestProductionOIDCIntegration' -count=1 -v

## verify-policy-dlp-audit: TASK-010 acceptance target. Provider-governance
## gates plus the versioned Policy/DLP effects journey: allow_with_redaction
## (provider sees redacted text), local_model_only, require_approval, deny,
## response pre-release scan and three reliable trace-linked audit records.
verify-policy-dlp-audit: verify-production-oidc integration-policy-dlp-audit
	@echo "verify-policy-dlp-audit: PASS"

integration-policy-dlp-audit:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestPolicyDLPAuditIntegration' -count=1 -v

## verify-mcp-governance: TASK-011 acceptance target. Policy/DLP gates plus
## the MCP central-governance journey: R1 direct execution, R2+ approval with
## zero upstream contact, one-time continuations, SSRF/redirect blocking and
## classified denial audits.
verify-mcp-governance: verify-policy-dlp-audit integration-mcp-governance
	@echo "verify-mcp-governance: PASS"

integration-mcp-governance:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestMCPGovernanceIntegration' -count=1 -v

## verify-knowledge-slice: TASK-012 acceptance target. Governance gates plus
## the Knowledge journey: upload->cited search in the authorized workspace,
## cross-workspace invisibility, DLP-blocked content, idempotent reupload
## and version replacement removing stale retrieval.
verify-knowledge-slice: verify-mcp-governance integration-knowledge-slice
	@echo "verify-knowledge-slice: PASS"

integration-knowledge-slice:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestKnowledgeSliceIntegration' -count=1 -v

## verify-llm-protocols: TASK-007 acceptance target. Governance gates plus
## the client-facing protocol surface: Models, Chat SSE, Responses,
## Embeddings and Messages under one fail-closed pipeline, with client
## cancellation and zero-egress DLP denial on every protocol.
verify-llm-protocols: verify-knowledge-slice integration-llm-protocols
	@echo "verify-llm-protocols: PASS"

integration-llm-protocols:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestLLMProtocolsIntegration' -count=1 -v

## verify-m2-m5: TASK-013 acceptance target — the complete capability and
## security gate: every slice gate chained plus the cross-plane journey
## (OIDC canonical Principal -> key -> LLM/MCP/Knowledge through one endpoint,
## fail-closed matrix with durable trace-linked evidence).
verify-m2-m5: verify-llm-protocols verify-mcp-governance verify-knowledge-slice integration-m2m5
	@echo "verify-m2-m5: PASS"

integration-m2m5:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestM2M5GateJourneyIntegration' -count=1 -v

## verify-device-bootstrap: TASK-015 acceptance target. M2-M5 gates plus the
## device flow journey: pending/denied/slow_down polling, approved login with
## one-time rotation, ETag bootstrap, logout revocation and fail-closed
## replay/removed-membership with audit evidence.
verify-device-bootstrap: verify-m2-m5 integration-device-bootstrap
	@echo "verify-device-bootstrap: PASS"

integration-device-bootstrap:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestDeviceBootstrapIntegration' -count=1 -v

## verify-skill-registry: TASK-016 acceptance target. Governance gates plus
## the Skill supply-chain journey: publish -> review -> signed release ->
## workspace-scoped download with offline-verifiable digest+signature, the
## full negative matrix, and key rotation preserving historical signatures.
verify-skill-registry: verify-device-bootstrap integration-skill-registry
	@echo "verify-skill-registry: PASS"

integration-skill-registry:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestSkillRegistryIntegration' -count=1 -v

## verify-eap-cli: TASK-017 acceptance target. Governance gates plus the CLI:
## unit coverage (precedence, redaction), in-process command journeys against
## the gateway contracts, and Windows/macOS/Linux cross-build qualification.
verify-eap-cli: verify-skill-registry cli-cross-build
	@echo "verify-eap-cli: PASS"

cli-cross-build:
	mkdir -p dist/cli
	GOOS=windows GOARCH=amd64 $(GO) build -o dist/cli/eap-windows-amd64.exe ./cmd/eap
	GOOS=darwin  GOARCH=amd64 $(GO) build -o dist/cli/eap-darwin-amd64 ./cmd/eap
	GOOS=darwin  GOARCH=arm64 $(GO) build -o dist/cli/eap-darwin-arm64 ./cmd/eap
	GOOS=linux   GOARCH=amd64 $(GO) build -o dist/cli/eap-linux-amd64 ./cmd/eap
	GOOS=linux   GOARCH=arm64 $(GO) build -o dist/cli/eap-linux-arm64 ./cmd/eap
	$(GO) test ./packages/cli/ -count=1

## verify-eap-skill-sync: TASK-018 acceptance target. Skill-registry gates
## plus the sync engine: install/upgrade/policy-removal convergence over
## managed files only, idempotent replay, interrupted-apply restore, and
## fail-closed tamper/malicious-archive/worker-failure paths.
verify-eap-skill-sync: verify-eap-cli
	$(GO) test ./packages/skillsync/ -count=1 -race
	@echo "verify-eap-skill-sync: PASS"

## verify-agent-distribution: TASK-019 acceptance target. Skill-sync gates
## plus the agent distribution journey: platform-matched listing, same-domain
## ticket issuance/redeem (single redemption), cross-workspace/wrong-version
## fail-closed and durable ticket records.
verify-agent-distribution: verify-eap-skill-sync integration-agent-distribution
	@echo "verify-agent-distribution: PASS"

integration-agent-distribution:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestAgentDistributionIntegration' -count=1 -v

## verify-portal-registry: TASK-020 acceptance target. Agent-distribution
## gates plus the Portal Marketplace journey: server-driven catalog,
## workspace-scoped search, non-maintainer denial server-side, and SPA/API
## precedence on the production gateway build.
verify-portal-registry: verify-agent-distribution integration-portal-registry
	@echo "verify-portal-registry: PASS"

integration-portal-registry:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestPortalRegistryIntegration' -count=1 -v

## verify-capability-gateway: TASK-034 acceptance target. Agent-distribution
## gates plus the capability meta-surface: permission-aware discovery,
## execute revalidation (approval/DLP/audit), dual-surface parity.
verify-capability-gateway: verify-agent-distribution integration-capability-gateway
	@echo "verify-capability-gateway: PASS"

integration-capability-gateway:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestCapabilityGateway' -count=1 -v

## verify-key-bound-workspace: TASK-035 acceptance target (ADR-0024).
## Foundation gates plus the workspace-bound key slice: header-less bound-key
## authorization, header > binding precedence, membership revalidation
## (revocation + cross-workspace), creation-time binding gate, audit origin.
verify-key-bound-workspace: verify-foundation integration-key-bound-workspace
	@echo "verify-key-bound-workspace: PASS"

integration-key-bound-workspace:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestAPIKey.*Binding' -count=1 -v

## verify-tool-passthrough: TASK-036 acceptance target (ADR-0025). Foundation
## gates plus OpenAI function calling through the routed path: tools/tool_choice
## passthrough, tool_calls round-trip (stream + non-stream), provider
## finish_reason, DLP scan of tool arguments, kernel-path fail-loud rejection.
verify-tool-passthrough: verify-foundation integration-tool-passthrough
	@echo "verify-tool-passthrough: PASS"

integration-tool-passthrough:
	$(COMPOSE) up -d postgres redis
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestTool' -count=1 -v
integration-tool-passthrough:
	$(COMPOSE) up -d postgres redis
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestTool' -count=1 -v

## verify-protocol-conformance: TASK-042 acceptance target (docs/04 section 5).
## Foundation gates plus the OpenAI/MCP conformance suite: error envelope,
## sampling parameter passthrough, tool_calls content-null serialization,
## Streamable-HTTP transport negotiation (Accept SSE, Mcp-Session-Id).
verify-protocol-conformance: verify-foundation integration-protocol-conformance
	@echo "verify-protocol-conformance: PASS"

integration-protocol-conformance:
	$(COMPOSE) up -d postgres redis
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestProtocol' -count=1 -v

## perf-gateway: TASK-033 performance gate. PRD §9 budget: gateway
## overhead P95 < 100ms with full governance enabled (DLP tokenize +
## exempt re-substitution + SSE window, policy, reliable audit). Reports a
## per-stage P50/P95/P99 table and fails on budget violations.
perf-gateway:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestGatewayPerformanceGate|TestGatewayConcurrencyGate' -count=1 -v
	@echo "perf-gateway: PASS"

## perf-concurrency-race: TASK-041 race gate. The concurrency harness runs
## under -race to prove no data races; timing assertions are skipped there
## (detector overhead skews latency) — the budget is asserted by perf-gateway.
perf-concurrency-race:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -race -tags=integration ./apps/gateway -run 'TestGatewayConcurrencyGate' -count=1 -v
	@echo "perf-concurrency-race: PASS"

## verify-admin-console: TASK-022 acceptance target. Portal-registry gates
## plus the admin console journey: fail-closed administrator role boundary,
## governed reads across users/agents/models/usage/MCP/knowledge/DLP/audit,
## per-read admin audit records, server-side pagination and secret-free
## payloads (no key material, credential refs, signatures or storage paths).
verify-admin-console: verify-portal-registry integration-admin-console
	@echo "verify-admin-console: PASS"

integration-admin-console:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestAdminConsoleIntegration' -count=1 -v

## verify-eap-v1: TASK-021 acceptance target — the complete EAP CLI and
## distribution V1 product gate. Chains every capability gate plus the
## product journey (device login -> skill lifecycle -> dual-client sync ->
## LLM/MCP/Knowledge through one key -> revocation closing all planes).
verify-eap-v1: verify-portal-registry integration-eap-v1
	@echo "verify-eap-v1: PASS"

integration-eap-v1:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestEAPV1ProductJourneyIntegration' -count=1 -v

## verify-portal-shell: TASK-005 acceptance target. Identity gates plus the
## Portal web workspace: typecheck/lint/format/unit tests, production build,
## bundle guard (no /runtime/agents) and Playwright browser flows.
verify-portal-shell: verify-identity-slice portal-e2e
	@echo "verify-portal-shell: PASS"

portal-check:
	cd apps/web && pnpm install --frozen-lockfile --prefer-offline && pnpm check

portal-e2e:
	./scripts/portal-e2e.sh

## verify-portal-deploy: TASK-007 acceptance target. Portal shell gates plus
## the deployment contract on the unified entry (compose image + headers).
verify-portal-deploy: verify-portal-shell portal-deploy-compose
	@echo "verify-portal-deploy: PASS"

portal-deploy-compose:
	$(COMPOSE) build --build-arg GOPROXY="$(GOPROXY)" gateway
	$(COMPOSE) up -d
	./scripts/dev-ready.sh
	PORTAL_BASE_URL=http://127.0.0.1:18080 ./scripts/portal-deploy-check.sh

## dev-ready-pre: internal helper — bring the gateway up for e2e when not running.
dev-ready-pre:
	@curl -fsS -o /dev/null http://127.0.0.1:18080/healthz 2>/dev/null || $(MAKE) --no-print-directory dev-up
	./scripts/dev-ready.sh


## verify-identity-slice: TASK-003 acceptance target. Runs the foundation gates
## plus the end-to-end identity flow against real PostgreSQL and Redis.
verify-identity-slice: verify-foundation integration-identity
	@echo "verify-identity-slice: PASS"

integration-identity:
	$(COMPOSE) up -d postgres redis
	@pg_cid=$$($(COMPOSE) ps -q postgres); \
	timeout=60; until docker exec $$pg_cid pg_isready -U eap -d eap >/dev/null 2>&1; do \
	  timeout=$$((timeout - 2)); [ $$timeout -le 0 ] && { echo "FAIL: postgres not healthy"; exit 1; }; sleep 2; done
	@redis_cid=$$($(COMPOSE) ps -q redis); \
	timeout=60; until docker exec $$redis_cid redis-cli ping >/dev/null 2>&1; do \
	  timeout=$$((timeout - 2)); [ $$timeout -le 0 ] && { echo "FAIL: redis not healthy"; exit 1; }; sleep 2; done
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16379" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestIdentitySlice' -count=1 -v

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

# Task worktrees under .tasks/ are ignored: they hold other owners'
# in-progress code and are gitignored anyway; main gates judge main code.
fmt-check:
	@if [ -n "$$(gofmt -l . | grep -v '^\.tasks/')" ]; then \
	  echo "unformatted Go files:"; gofmt -l . | grep -v '^\.tasks/'; exit 1; \
	fi

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

run-gateway:
	$(GO) run ./apps/gateway

## dev-infra: start PostgreSQL, Redis and MinIO only (run the gateway from source).
dev-infra:
	$(COMPOSE) up -d postgres redis minio

## dev-up: start the full local environment (PostgreSQL, Redis, MinIO, gateway).
dev-up:
	$(COMPOSE) build --build-arg GOPROXY="$(GOPROXY)" gateway
	$(COMPOSE) up -d

## dev-ready: wait until the gateway reports healthy and ready.
dev-ready:
	./scripts/dev-ready.sh

dev-down:
	$(COMPOSE) down

## dev-nuke: stop and delete local volumes (destructive, dev only).
dev-nuke:
	$(COMPOSE) down -v

# TASK-049: soak & resource stability acceptance target. Runs against the
# dedicated TASK-049 verification containers on ports 15435/16382 — shared
# dev infra (eap-dev-*, 15432/16379) and sibling verification stacks
# (15433/16380, 15434/16381) are deliberately NOT touched: a 30-minute
# sustained run must not fight other suites for its database. Duration is
# configurable via SOAK_DURATION (Go duration; default 30m is the acceptance
# profile, SOAK_DURATION=2m is the mechanic validation). The full run needs a
# generous go-test timeout; containers are created if missing and left up for
# re-runs.
.PHONY: soak-gateway
soak-gateway:
	@docker inspect eap-verify-pg-049 >/dev/null 2>&1 || \
	  docker run -d --name eap-verify-pg-049 -e POSTGRES_USER=eap -e POSTGRES_PASSWORD=eap -p 15435:5432 postgres:16-alpine
	@docker inspect eap-verify-redis-049 >/dev/null 2>&1 || \
	  docker run -d --name eap-verify-redis-049 -p 16382:6379 redis:7-alpine
	@timeout=60; until docker exec eap-verify-pg-049 pg_isready -U eap -d postgres >/dev/null 2>&1; do \
	  timeout=$$((timeout - 2)); [ $$timeout -le 0 ] && { echo "FAIL: TASK-049 postgres not ready"; exit 1; }; sleep 2; done
	@timeout=60; until docker exec eap-verify-redis-049 redis-cli ping >/dev/null 2>&1; do \
	  timeout=$$((timeout - 2)); [ $$timeout -le 0 ] && { echo "FAIL: TASK-049 redis not ready"; exit 1; }; sleep 2; done
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15435/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16382" \
	SOAK_DURATION="$(SOAK_DURATION)" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestGatewaySoak' -count=1 -v -timeout 45m

.PHONY: verify-failure-modes

## verify-failure-modes: TASK-046 acceptance target. The failure-mode &
## degradation matrix against the REAL stack with infrastructure-level fault
## injection: PG unavailable fails closed with a distinct storage-error class
## (audit delivery failure logged loudly), Redis unavailable fails rate
## limiting OPEN with everything else working, exhausted provider failover
## yields a deterministic EAP-wrapped error without credential/URL material,
## mid-stream provider death ends in a terminal SSE error frame with the
## audit trail recorded, and audit delivery failure withholds the answer.
## Drain checks after every scenario (no goroutine growth, settled pgxpool).
##
## The containers below are the TASK-046 qualification containers. They are
## ISOLATED from the shared dev stack (15432/16379) and from sibling task
## containers — this suite stops/starts them mid-run, so the ports 15433
## and 16380 are hardcoded here and guarded inside the tests.
verify-failure-modes:
	@if [ "$$(docker inspect -f '{{.State.Status}}' eap-verify-pg-046 2>/dev/null)" != "running" ]; then \
	  if [ -z "$$(docker inspect -f '{{.State.Status}}' eap-verify-pg-046 2>/dev/null)" ]; then \
	    docker run -d --name eap-verify-pg-046 -e POSTGRES_USER=eap -e POSTGRES_PASSWORD=eap -p 15433:5432 postgres:16-alpine; \
	  else \
	    docker start eap-verify-pg-046; \
	  fi; \
	fi
	@timeout=60; until docker exec eap-verify-pg-046 pg_isready -U eap >/dev/null 2>&1; do \
	  timeout=$$((timeout - 2)); [ $$timeout -le 0 ] && { echo "FAIL: eap-verify-pg-046 not healthy"; exit 1; }; sleep 2; done
	@if [ "$$(docker inspect -f '{{.State.Status}}' eap-verify-redis-046 2>/dev/null)" != "running" ]; then \
	  if [ -z "$$(docker inspect -f '{{.State.Status}}' eap-verify-redis-046 2>/dev/null)" ]; then \
	    docker run -d --name eap-verify-redis-046 -p 16380:6379 redis:7-alpine; \
	  else \
	    docker start eap-verify-redis-046; \
	  fi; \
	fi
	@timeout=30; until docker exec eap-verify-redis-046 redis-cli ping >/dev/null 2>&1; do \
	  timeout=$$((timeout - 2)); [ $$timeout -le 0 ] && { echo "FAIL: eap-verify-redis-046 not healthy"; exit 1; }; sleep 2; done
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15433/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16380" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestFailureModes' -count=1 -v
	@echo "verify-failure-modes: PASS"

.PHONY: verify-security-adversarial

## verify-security-adversarial: TASK-047 acceptance target. Adversarial
## security suite against an isolated stack (TASK-047 containers on
## 15434/16381 — never the shared dev compose): authorization matrix sweep
## generated from the live route inventory, credential-leakage scan over the
## real log stream and all traffic, DLP bypass corpus with zero-leak
## assertions, and rate-limit effectiveness (429 shape + recovery).
## Start the isolated stack first:
##   docker run -d --name eap-verify-pg-047 -e POSTGRES_USER=eap -e POSTGRES_PASSWORD=eap -p 15434:5432 postgres:16-alpine
##   docker run -d --name eap-verify-redis-047 -p 16381:6379 redis:7-alpine
verify-security-adversarial:
	EAP_TEST_DATABASE_URL="postgres://eap:eap@127.0.0.1:15434/postgres?sslmode=disable" \
	EAP_TEST_REDIS_ADDR="127.0.0.1:16381" \
	$(GO) test -tags=integration ./apps/gateway -run 'TestAuthzMatrix|TestCredentialLeakage|TestDLPBypassCorpus|TestDLPRuleFindings|TestRateLimitEffectiveness' -count=1 -v
	@echo "verify-security-adversarial: PASS"
