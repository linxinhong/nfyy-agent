#!/usr/bin/env bash
# Run a formally bound memory-gate-load command and always preserve its exit.
#
# A successful child result without a durable, readable sidecar is unusable as
# formal evidence. Keep evidence failures distinct from the child status.
set -euo pipefail

evidence_failure_exit=70
signal_exit=71
runner_pid=''
child_active=0
cleanup_in_progress=0
launching=0
pending_signal=''

cleanup_active_child() {
  if [ "$child_active" -ne 1 ] || [ -z "$runner_pid" ]; then
    return
  fi
  if [ "$cleanup_in_progress" -eq 1 ]; then
    return
  fi

  cleanup_in_progress=1
  # A second termination signal must not interrupt child reap and leave the
  # destructive runner alive.
  trap '' HUP INT TERM

  if kill -0 "$runner_pid" 2>/dev/null; then
    kill -TERM "$runner_pid" 2>/dev/null || :
  fi
  local attempts=0
  while kill -0 "$runner_pid" 2>/dev/null && [ "$attempts" -lt 50 ]; do
    sleep 0.1 || :
    attempts=$((attempts + 1))
  done
  if kill -0 "$runner_pid" 2>/dev/null; then
    kill -KILL "$runner_pid" 2>/dev/null || :
  fi
  # A wait in a conditional is immune to errexit and reliably reaps either a
  # TERM/KILL result or a child that raced to normal completion.
  if wait "$runner_pid"; then
    :
  else
    :
  fi
  child_active=0
}

on_signal() {
  local signal_name=$1
  # Bash may dispatch a trap after `COMMAND &` has created the child but
  # before `$!` is assigned. Do not exit in that interval: remember the signal
  # and terminate/reap immediately once the PID has been made active below.
  if [ "$launching" -eq 1 ]; then
    pending_signal=$signal_name
    return
  fi
  printf 'memory-gate-formal-wrapper: interrupted signal=%s\n' "$signal_name" >&2 || :
  cleanup_active_child
  exit "$signal_exit"
}

on_exit() {
  local wrapper_status=$?
  trap - EXIT
  if [ "$launching" -eq 1 ] && [ -n "$runner_pid" ]; then
    child_active=1
  fi
  if [ "$child_active" -eq 1 ]; then
    cleanup_active_child
    # No path with an unreaped child may be represented as wrapper success.
    if [ "$wrapper_status" -eq 0 ]; then
      wrapper_status=$evidence_failure_exit
    fi
  fi
  exit "$wrapper_status"
}

launch_barrier_for_test() {
  # This test-only, opt-in hook pauses exactly after `COMMAND &` and before
  # reading `$!`. It is intentionally unused by formal invocations.
  local barrier=${MEMORY_GATE_FORMAL_TEST_LAUNCH_BARRIER:-}
  if [ -z "$barrier" ]; then
    return
  fi
  : > "${barrier}.ready"
  while [ ! -e "${barrier}.continue" ]; do
    sleep 0.01
  done
}

trap on_exit EXIT
trap 'on_signal HUP' HUP
trap 'on_signal INT' INT
trap 'on_signal TERM' TERM

evidence_failure() {
  local step=$1
  # stderr itself may be unavailable, but never allow that to hide the
  # dedicated non-child failure status.
  printf 'memory-gate-formal-wrapper: evidence_write_failed step=%s\n' "$step" >&2 || :
  exit "$evidence_failure_exit"
}

sync_path() {
  local path=$1
  # Ruby is present on supported macOS hosts and gives a true fsync(2). The
  # sync(8) fallback is weaker but still requests a system-wide flush on hosts
  # without Ruby; in either case a failure rejects the evidence.
  if command -v ruby >/dev/null 2>&1; then
    ruby -e 'File.open(ARGV.fetch(0), "r") { |file| file.fsync }' "$path"
    return
  fi
  sync
}

write_process_start() {
  if ! printf 'runner_pid=%s\nstarted_at=%s\n' "$runner_pid" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$artifact_dir/runner-process.txt"; then
    evidence_failure process_start
  fi
  if ! sync_path "$artifact_dir/runner-process.txt"; then
    evidence_failure process_start_fsync
  fi
}

write_exit_sidecar() {
  local temporary_exit="$artifact_dir/.runner.exit.${runner_pid}.${RANDOM}.tmp"
  if ! printf '%s\n' "$exit_code" > "$temporary_exit"; then
    evidence_failure exit_temporary
  fi
  if ! sync_path "$temporary_exit"; then
    evidence_failure exit_temporary_fsync
  fi
  if ! mv "$temporary_exit" "$artifact_dir/runner.exit"; then
    evidence_failure exit_rename
  fi
  if ! sync_path "$artifact_dir"; then
    evidence_failure exit_directory_fsync
  fi

  local recorded_exit
  if ! recorded_exit=$(<"$artifact_dir/runner.exit") || ! [[ "$recorded_exit" =~ ^[0-9]+$ ]] || [ "$recorded_exit" != "$exit_code" ]; then
    evidence_failure exit_verify
  fi
}

write_process_end() {
  if ! printf 'runner_pid=%s\nended_at=%s\nexit_code=%s\n' "$runner_pid" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$exit_code" >> "$artifact_dir/runner-process.txt"; then
    evidence_failure process_end
  fi
  if ! sync_path "$artifact_dir/runner-process.txt"; then
    evidence_failure process_end_fsync
  fi
}

if [ "$#" -lt 3 ] || [ "$2" != "--" ]; then
  echo "usage: $0 ARTIFACT_DIRECTORY -- COMMAND [ARG ...]" >&2
  exit 2
fi

artifact_dir=$1
shift 2
case $artifact_dir in
  *$'\n'*|*$'\r'*)
    echo "artifact directory contains a control character" >&2
    exit 2
    ;;
esac
if ! mkdir -p "$artifact_dir"; then
  evidence_failure mkdir
fi
for artifact in runner.stdout.txt runner.stderr.txt runner.exit runner-process.txt; do
  if [ -e "$artifact_dir/$artifact" ]; then
    echo "refusing to overwrite runner artifact: $artifact_dir/$artifact" >&2
    exit 1
  fi
done

# Pre-create both redirected streams so a file-system failure is detected by
# this wrapper rather than becoming an ambiguous child status.
if ! : > "$artifact_dir/runner.stdout.txt"; then
  evidence_failure runner_stdout
fi
if ! : > "$artifact_dir/runner.stderr.txt"; then
  evidence_failure runner_stderr
fi

launching=1
"$@" >"$artifact_dir/runner.stdout.txt" 2>"$artifact_dir/runner.stderr.txt" &
launch_barrier_for_test
runner_pid=$!
child_active=1
launching=0
if [ -n "$pending_signal" ]; then
  on_signal "$pending_signal"
fi
write_process_start
if wait "$runner_pid"; then
  exit_code=0
else
  exit_code=$?
fi
child_active=0
write_exit_sidecar
write_process_end
exit "$exit_code"
