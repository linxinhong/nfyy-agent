#!/usr/bin/env bash
# Regression tests for outer disposable-container cleanup status handling.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
launcher="$repo_root/scripts/run-memory-gate-container.sh"
test_root=$(mktemp -d "${TMPDIR:-/tmp}/memory-gate-container-wrapper-test.XXXXXX")

cleanup() {
  rm -rf "$test_root"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

expect_exit() {
  local expected=$1
  shift
  set +e
  "$@"
  local actual=$?
  set -e
  [ "$actual" -eq "$expected" ] || fail "expected exit $expected, got $actual"
}

fake_bin="$test_root/fake-bin"
mkdir "$fake_bin"
cat > "$fake_bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MEMORY_GATE_CONTAINER_DOCKER_LOG"
if [ "${MEMORY_GATE_CONTAINER_DOCKER_FAIL:-}" = 1 ]; then
  exit 1
fi
EOF
chmod +x "$fake_bin/docker"

docker_log="$test_root/docker.log"
PATH="$fake_bin:$PATH" MEMORY_GATE_CONTAINER_DOCKER_LOG="$docker_log" expect_exit 0 "$launcher" "$test_root/zero" disposable-pg -- sh -c 'exit 0'
rg -qx 'stop disposable-pg' "$docker_log" || fail "successful child did not stop exact container"

PATH="$fake_bin:$PATH" MEMORY_GATE_CONTAINER_DOCKER_LOG="$docker_log" expect_exit 7 "$launcher" "$test_root/seven" disposable-pg -- sh -c 'exit 7'

PATH="$fake_bin:$PATH" MEMORY_GATE_CONTAINER_DOCKER_LOG="$docker_log" MEMORY_GATE_CONTAINER_DOCKER_FAIL=1 expect_exit 70 "$launcher" "$test_root/cleanup-failure" disposable-pg -- sh -c 'exit 0'

printf 'run-memory-gate-container wrapper tests: PASS\n'
