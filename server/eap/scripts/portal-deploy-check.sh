#!/usr/bin/env bash
# Asserts Portal deployment behavior on the unified entry (TASK-007):
# UI at the root, SPA fallback on refresh, API precedence with JSON 404s,
# security headers and cache policy — all against a RUNNING gateway.
set -euo pipefail
BASE="${PORTAL_BASE_URL:-http://127.0.0.1:18081}"

fail() { echo "FAIL: $1" >&2; exit 1; }
is_html() { grep -qi "<!doctype html\|<html" <<< "$1"; }

body=$(curl -fsS "$BASE/") || fail "GET / failed"
is_html "$body" || fail "/ did not return the portal HTML"

body=$(curl -fsS "$BASE/portal/keys") || fail "GET /portal/keys failed"
is_html "$body" || fail "/portal/keys refresh did not return the SPA shell"

# Unknown API path: 404 JSON, never the HTML shell.
probe=$(curl -sS -o /tmp/eap-deploy-404 -w "%{http_code} %{content_type}" "$BASE/enterprise/v1/definitely-missing") \
  || fail "unknown API probe failed"
grep -q "404" <<<"$probe" || fail "unknown API must 404, got: $probe"
grep -q "application/json" <<<"$probe" || fail "unknown API 404 must be JSON, got: $probe"
grep -qi "<html" /tmp/eap-deploy-404 && fail "unknown API 404 must not be HTML"
echo "PASS: unknown API returns JSON 404 ($probe)"

# Security headers on the shell.
headers=$(curl -sSI "$BASE/")
for want in "Content-Security-Policy" "X-Content-Type-Options: nosniff" "X-Frame-Options: DENY" "Referrer-Policy"; do
  grep -qi "$want" <<<"$headers" || fail "missing header: $want"
done
echo "PASS: security headers present (CSP, nosniff, frame-options, referrer-policy)"

# Cache policy: shell no-cache, hashed assets immutable.
shell_cc=$(grep -i "^cache-control:" <<<"$headers" | head -1)
grep -qi "no-cache" <<<"$shell_cc" || fail "shell cache-control must be no-cache, got: $shell_cc"

asset_path=$(grep -o 'assets/[^"]*\.js' <<<"$body" | head -1 || true)
if [ -n "$asset_path" ]; then
  asset_headers=$(curl -sSI "$BASE/$asset_path")
  asset_cc=$(grep -i "^cache-control:" <<<"$asset_headers" | head -1)
  grep -qi "immutable" <<<"$asset_cc" || fail "hashed assets must be immutable, got: $asset_cc"
  echo "PASS: hashed assets immutable ($asset_path)"
else
  echo "SKIP: no js asset referenced in index (unexpected for a built bundle)"
  exit 1
fi

echo "portal-deploy-check: PASS against $BASE"
