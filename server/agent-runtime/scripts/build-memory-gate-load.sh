#!/usr/bin/env bash
# Build a formally bound memory-gate-load binary from a clean fixed commit.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 OUTPUT_DIRECTORY" >&2
  exit 2
fi

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"
case $1 in
  /*) output_dir=$1 ;;
  *) output_dir="$repo_root/$1" ;;
esac
mkdir -p "$(dirname "$output_dir")"
output_dir=$(cd "$(dirname "$output_dir")" && pwd -P)/$(basename "$output_dir")
case $output_dir in
  "$repo_root"/*)
    echo "formal build output must be outside the source worktree" >&2
    exit 1
    ;;
esac
if [ -n "$(git status --porcelain=v1)" ]; then
  echo "refusing to build formal runner from a dirty worktree" >&2
  exit 1
fi

commit=$(git rev-parse HEAD)
runner_hash=$(shasum -a 256 cmd/memory-gate-load/main.go | awk '{print $1}')
product_hash=$(shasum -a 256 pkg/db/postgres/trusted_memory_store.go | awk '{print $1}')
mkdir -p "$output_dir"
binary="$output_dir/memory-gate-load"
for artifact in memory-gate-load memory-gate-load.build-command.txt memory-gate-load.build.stdout.txt memory-gate-load.buildinfo.txt memory-gate-load.binary.sha256 memory-gate-load.inputs.txt memory-gate-load.build-environment.txt; do
  if [ -e "$output_dir/$artifact" ]; then
    echo "refusing to overwrite existing formal build artifact: $output_dir/$artifact" >&2
    exit 1
  fi
done
ldflags="-X main.buildCommit=$commit -X main.buildRunnerSourceSHA256=$runner_hash -X main.buildProductSHA256=$product_hash"
go_env=(env -i "PATH=$PATH" "HOME=$HOME" "TMPDIR=${TMPDIR:-/tmp}" GOENV=off GOWORK=off GOFLAGS= CGO_ENABLED=1 GOPROXY=off GOSUMDB=off GONOSUMDB=)
printf 'GOENV=off\nGOWORK=off\nGOFLAGS=\nCGO_ENABLED=1\nGOPROXY=off\nGOSUMDB=off\nGONOSUMDB=\nPATH=<inherited>\nHOME=<inherited>\nTMPDIR=%s\n' "${TMPDIR:-/tmp}" > "$output_dir/memory-gate-load.build-environment.txt"

# Go does not stamp vcs.* settings when it builds from this repository's
# linked worktree on the current host. Build a clean temporary physical clone
# at the already-fixed commit so -buildvcs=true has an unambiguous .git dir.
build_source=$(mktemp -d "${TMPDIR:-/tmp}/memory-gate-build-source.XXXXXX")
trap 'rm -rf "$build_source"' EXIT
git clone --quiet --no-local "$repo_root" "$build_source"
git -C "$build_source" checkout --quiet "$commit"
if [ -n "$(git -C "$build_source" status --porcelain=v1)" ]; then
  echo "temporary formal build source is dirty" >&2
  exit 1
fi
if [ "$(shasum -a 256 "$build_source/cmd/memory-gate-load/main.go" | awk '{print $1}')" != "$runner_hash" ] || [ "$(shasum -a 256 "$build_source/pkg/db/postgres/trusted_memory_store.go" | awk '{print $1}')" != "$product_hash" ]; then
  echo "temporary build source hashes do not match fixed worktree" >&2
  exit 1
fi

printf '%q ' "${go_env[@]}" go -C "$build_source" build -trimpath -buildvcs=true -ldflags "$ldflags" -o "$binary" ./cmd/memory-gate-load > "$output_dir/memory-gate-load.build-command.txt"
printf '\n' >> "$output_dir/memory-gate-load.build-command.txt"
"${go_env[@]}" go -C "$build_source" build -trimpath -buildvcs=true -ldflags "$ldflags" -o "$binary" ./cmd/memory-gate-load > "$output_dir/memory-gate-load.build.stdout.txt" 2>&1
"${go_env[@]}" go version -m "$binary" > "$output_dir/memory-gate-load.buildinfo.txt"
if ! rg -q $'build\\tvcs.revision='"$commit"'$' "$output_dir/memory-gate-load.buildinfo.txt" || ! rg -q $'build\\tvcs.modified=false$' "$output_dir/memory-gate-load.buildinfo.txt"; then
  echo "formal binary is missing required immutable VCS build settings" >&2
  exit 1
fi
shasum -a 256 "$binary" > "$output_dir/memory-gate-load.binary.sha256"
printf 'commit=%s\nrunner_source_sha256=%s\nproduct_sha256=%s\n' "$commit" "$runner_hash" "$product_hash" > "$output_dir/memory-gate-load.inputs.txt"
