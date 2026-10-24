#!/usr/bin/env bash
# Run an already-bound formal command and always stop its disposable --rm PG.
# This outer helper deliberately uses exit_code: `status` is a read-only zsh
# special parameter and previously obscured a completed launcher cleanup.
set -euo pipefail

cleanup_failure_exit=70
container_name=''
artifact_dir=''
container_cleaned=0

cleanup_container() {
  local exit_code=$?
  trap - EXIT HUP INT TERM
  if [ -n "$container_name" ] && [ "$container_cleaned" -eq 0 ]; then
    if ! docker stop "$container_name" > "$artifact_dir/docker-stop.txt" 2>&1; then
      printf 'memory-gate-container-wrapper: cleanup_failed\n' >&2 || :
      if [ "$exit_code" -eq 0 ]; then
        exit_code=$cleanup_failure_exit
      fi
    else
      container_cleaned=1
    fi
  fi
  exit "$exit_code"
}

if [ "$#" -lt 4 ] || [ "$3" != "--" ]; then
  echo "usage: $0 ARTIFACT_DIRECTORY DISPOSABLE_CONTAINER -- COMMAND [ARG ...]" >&2
  exit 2
fi

artifact_dir=$1
container_name=$2
shift 3
case $artifact_dir$container_name in
  *$'\n'*|*$'\r'*)
    echo "artifact directory or container name contains a control character" >&2
    exit 2
    ;;
esac
if ! mkdir -p "$artifact_dir"; then
  echo "memory-gate-container-wrapper: artifact_directory_failed" >&2
  exit "$cleanup_failure_exit"
fi

trap cleanup_container EXIT
trap 'exit 71' HUP INT TERM
"$@"
