#!/usr/bin/env bash
# Regression tests for the formal runner evidence wrapper.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
wrapper="$repo_root/scripts/run-memory-gate-formal.sh"
test_root=$(mktemp -d "${TMPDIR:-/tmp}/memory-gate-wrapper-test.XXXXXX")

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
  [ "$actual" -eq "$expected" ] || fail "expected exit $expected, got $actual: $*"
}

expect_wait_exit() {
  local expected=$1
  local pid=$2
  set +e
  wait "$pid"
  local actual=$?
  set -e
  [ "$actual" -eq "$expected" ] || fail "expected waited pid $pid exit $expected, got $actual"
}

assert_sidecar() {
  local artifact_dir=$1
  local expected=$2
  [ -f "$artifact_dir/runner.exit" ] || fail "missing runner.exit"
  [ "$(<"$artifact_dir/runner.exit")" = "$expected" ] || fail "unexpected runner.exit"
  rg -q "^runner_pid=[0-9]+$" "$artifact_dir/runner-process.txt" || fail "missing process start"
  rg -q "^exit_code=${expected}$" "$artifact_dir/runner-process.txt" || fail "missing process end"
}

success_dir="$test_root/child-zero"
expect_exit 0 "$wrapper" "$success_dir" -- sh -c 'exit 0'
assert_sidecar "$success_dir" 0

failure_dir="$test_root/child-seven"
expect_exit 7 "$wrapper" "$failure_dir" -- sh -c 'exit 7'
assert_sidecar "$failure_dir" 7

signal_dir="$test_root/child-signal"
expect_exit 143 "$wrapper" "$signal_dir" -- sh -c 'kill -TERM $$'
assert_sidecar "$signal_dir" 143

# A non-directory target makes mkdir fail deterministically even if the test
# is executed as root. The wrapper must not translate this evidence failure to
# a successful child exit.
blocked_path="$test_root/blocked"
: > "$blocked_path"
expect_exit 70 "$wrapper" "$blocked_path" -- sh -c 'exit 0'

# Simulate a post-child write failure: child success must not mask the failed
# final process append, and the atomic exit sidecar remains available.
end_write_dir="$test_root/process-end-write-failure"
export artifact_dir_for_test="$end_write_dir"
expect_exit 70 "$wrapper" "$end_write_dir" -- sh -c 'rm "$artifact_dir_for_test/runner-process.txt"; mkdir "$artifact_dir_for_test/runner-process.txt"; exit 0'
[ "$(<"$end_write_dir/runner.exit")" = 0 ] || fail "child exit sidecar was not retained"

# Pause the wrapper after child launch but before the first process evidence
# append. Turning that target into a directory makes the append fail
# deterministically; the sleeping child must be TERM/KILL reaped before the
# wrapper reports its dedicated evidence failure.
fake_bin="$test_root/fake-bin"
mkdir "$fake_bin"
date_ready="$test_root/date-ready"
date_continue="$test_root/date-continue"
cat > "$fake_bin/date" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
: > "$MEMORY_GATE_WRAPPER_DATE_READY"
while [ ! -e "$MEMORY_GATE_WRAPPER_DATE_CONTINUE" ]; do
  sleep 0.01
done
exec /bin/date "$@"
EOF
chmod +x "$fake_bin/date"
prewait_dir="$test_root/post-launch-process-start-failure"
child_pid_file="$test_root/prewait-child.pid"
MEMORY_GATE_WRAPPER_DATE_READY="$date_ready" MEMORY_GATE_WRAPPER_DATE_CONTINUE="$date_continue" PATH="$fake_bin:$PATH" "$wrapper" "$prewait_dir" -- sh -c 'printf "%s\\n" "$$" > "$1"; exec sleep 30' sh "$child_pid_file" &
prewait_wrapper_pid=$!
for _ in $(seq 1 500); do
  [ -e "$date_ready" ] && [ -s "$child_pid_file" ] && break
  sleep 0.01
done
[ -e "$date_ready" ] || fail "wrapper did not reach process-start audit"
child_pid=$(<"$child_pid_file")
mkdir "$prewait_dir/runner-process.txt"
: > "$date_continue"
expect_wait_exit 70 "$prewait_wrapper_pid"
if kill -0 "$child_pid" 2>/dev/null; then
  fail "post-launch process-start failure leaked child $child_pid"
fi

# A TERM sent to the wrapper while the child sleeps must be forwarded and
# reaped. The wrapper's signal status is intentionally non-zero.
term_dir="$test_root/wrapper-term"
term_child_pid_file="$test_root/term-child.pid"
"$wrapper" "$term_dir" -- sh -c 'printf "%s\\n" "$$" > "$1"; exec sleep 30' sh "$term_child_pid_file" &
term_wrapper_pid=$!
for _ in $(seq 1 500); do
  [ -s "$term_child_pid_file" ] && break
  sleep 0.01
done
[ -s "$term_child_pid_file" ] || fail "TERM test child did not start"
term_child_pid=$(<"$term_child_pid_file")
kill -TERM "$term_wrapper_pid"
expect_wait_exit 71 "$term_wrapper_pid"
if kill -0 "$term_child_pid" 2>/dev/null; then
  fail "TERM wrapper leaked child $term_child_pid"
fi

# Pause exactly after `COMMAND &` and before the wrapper reads `$!`. TERM in
# that launch window is deferred until the PID becomes active, then the child
# must be reaped and the wrapper must remain non-successful.
launch_dir="$test_root/launch-signal-race"
launch_barrier="$test_root/launch-barrier"
launch_child_pid_file="$test_root/launch-child.pid"
MEMORY_GATE_FORMAL_TEST_LAUNCH_BARRIER="$launch_barrier" "$wrapper" "$launch_dir" -- sh -c 'printf "%s\\n" "$$" > "$1"; exec sleep 30' sh "$launch_child_pid_file" &
launch_wrapper_pid=$!
for _ in $(seq 1 500); do
  [ -e "${launch_barrier}.ready" ] && [ -s "$launch_child_pid_file" ] && break
  sleep 0.01
done
[ -e "${launch_barrier}.ready" ] || fail "wrapper did not reach launch barrier"
[ -s "$launch_child_pid_file" ] || fail "launch-race child did not start"
launch_child_pid=$(<"$launch_child_pid_file")
kill -TERM "$launch_wrapper_pid"
: > "${launch_barrier}.continue"
expect_wait_exit 71 "$launch_wrapper_pid"
if kill -0 "$launch_child_pid" 2>/dev/null; then
  fail "launch-window TERM leaked child $launch_child_pid"
fi

printf 'run-memory-gate-formal wrapper tests: PASS\n'
