# Gateway overhead benchmark (TASK-033, PRD §9)

Run: `make perf-gateway` (real middleware chain, real PostgreSQL/Redis,
external provider stubbed; 200 samples per stage, single machine, dockerized PG/Redis on localhost).

## Results — 2026-09-03, main @ perf commit (Apple Silicon, docker PG/Redis)

| Stage | n | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| dlp_scan_12kb_tokenize (12KB payload, ~20 phone hits, tokenize + vault writes) | 200 | 5.63ms | 7.93ms | 11.41ms |
| vault_resub_1token (single-token reveal) | 200 | 0.23ms | 0.74ms | 1.24ms |
| e2e_chat_full_chain (auth+workspace+policy+DLP+audit+token exchange+LLM stub) | 200 | 9.64ms | 12.19ms | 16.30ms |
| e2e_chat_dlp_off (same chain, DLP engine neutralized) | 200 | 4.57ms | 7.56ms | 16.14ms |

- **DLP-added overhead (p95 delta)**: 4.62ms
- **SSE window throughput**: ~121,000 chunks/sec (500-chunk probe)
- **PRD §9 budget**: gateway overhead p95 < 100ms → measured full-chain p95 12.19ms, **~8× headroom**

## Interpretation

- The full governance chain costs ~12ms p95 end-to-end on a single local
  instance; the DLP layer (tokenize + vault + re-substitution) accounts for
  ~4.6ms of that. Audit writes and token exchange ride the same PostgreSQL
  round-trips as the control data reads.
- The provider stub contributes ~1ms; provider inference time is excluded
  from the gateway-overhead definition by PRD §9.
- Fail-closed paths (deny/withhold) are cheaper than tokenize (no vault
  write), so these numbers are an upper bound of DLP cost.

## Reproduce

```bash
docker compose -f deploy/compose.yaml up -d  # PG :15432, Redis :16379
make perf-gateway
```
