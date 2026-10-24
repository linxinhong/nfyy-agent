#!/usr/bin/env bash
# Builds the CLI-generated Portal, hosts it on the gateway (:18081) and runs
# Playwright against the unified entry — no Vite dev server (ADR-0007).
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$(pwd)"

GO=${GO:-go}
COMPOSE="docker compose -f deploy/docker-compose.yml"

cd "$ROOT/apps/web" && pnpm exec vite build && cd "$ROOT"
if grep -rq "runtime/agents" "$ROOT/apps/web/dist"; then
  echo "FAIL: production bundle contains the forbidden /runtime/agents contract" >&2
  exit 1
fi
$COMPOSE up -d postgres redis
pg_cid=$($COMPOSE ps -q postgres)
timeout=60
until docker exec "$pg_cid" pg_isready -U eap -d eap >/dev/null 2>&1; do
  timeout=$((timeout - 2)); [ "$timeout" -le 0 ] && { echo "FAIL: postgres not healthy" >&2; exit 1; }
  sleep 2
done

lsof -ti tcp:18081 2>/dev/null | xargs kill 2>/dev/null || true
$GO build -o /tmp/eap-portal-gateway ./apps/gateway
EAP_GATEWAY_ADDR=":18081" \
EAP_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/eap?sslmode=disable" \
EAP_REDIS_ADDR="127.0.0.1:16379" \
EAP_DEV_MODE=1 \
EAP_WEB_DIST="apps/web/dist" \
/tmp/eap-portal-gateway >/tmp/eap-portal-e2e-gateway.log 2>&1 &
gw_pid=$!
cleanup() { kill "$gw_pid" 2>/dev/null || true; }
trap cleanup EXIT

timeout=30
until curl -fsS http://127.0.0.1:18081/healthz >/dev/null 2>&1; do
  timeout=$((timeout - 1)); [ "$timeout" -le 0 ] && { echo "FAIL: portal gateway not ready" >&2; exit 1; }
  sleep 1
done

cd "$ROOT/apps/web"
pnpm e2e
