#!/usr/bin/env bash
# Asserts that the local development environment is healthy:
# the gateway answers /healthz and /readyz (which probes PostgreSQL and Redis).
set -euo pipefail

GATEWAY_URL="${EAP_GATEWAY_URL:-http://127.0.0.1:18080}"
TIMEOUT_SECONDS="${EAP_READY_TIMEOUT:-120}"
deadline=$((SECONDS + TIMEOUT_SECONDS))

check() { curl -fsS -o /dev/null "$1"; }

echo "waiting for gateway readiness at ${GATEWAY_URL} (timeout ${TIMEOUT_SECONDS}s) ..."
until check "${GATEWAY_URL}/healthz"; do
  if (( SECONDS >= deadline )); then
    echo "FAIL: /healthz did not become ready in time" >&2
    docker compose -f "$(dirname "$0")/../deploy/docker-compose.yml" ps >&2 || true
    exit 1
  fi
  sleep 2
done
echo "PASS: /healthz"

until check "${GATEWAY_URL}/readyz"; do
  if (( SECONDS >= deadline )); then
    echo "FAIL: /readyz did not become ready in time" >&2
    curl -sS "${GATEWAY_URL}/readyz" >&2 || true
    exit 1
  fi
  sleep 2
done

echo "PASS: /readyz"
echo "--- readiness body ---"
curl -fsS "${GATEWAY_URL}/readyz"
echo
