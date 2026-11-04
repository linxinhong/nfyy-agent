#!/usr/bin/env bash
# Regenerate requirements-macos-arm64.txt from requirements.txt's pinned closure.
# Runs on any OS: wheels are fetched for macOS arm64 / CPython 3.12 without installing them.
# Review the diff before committing; every hash change is a supply-chain change.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
closure="${1:-$here/requirements-closure.txt}"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
python3 -m pip download -q --only-binary=:all: --no-deps \
  --platform macosx_11_0_arm64 --python-version 3.12 --implementation cp --abi cp312 \
  -r "$closure" -d "$work/wheels"
{
  echo "# WordBridge runtime closure for macOS arm64 / CPython 3.12, hash-pinned."
  echo "# Regenerate with apps/wordbridge/scripts/lock-macos-arm64.sh; installed with --require-hashes --no-deps."
  python3 -I - "$work/wheels" <<'PY'
import hashlib, pathlib, sys
from pip._vendor.packaging.utils import parse_wheel_filename
for whl in sorted(pathlib.Path(sys.argv[1]).glob("*.whl"), key=lambda p: p.name.lower()):
    name, version, _, _ = parse_wheel_filename(whl.name)
    print(f"{name}=={version} \\\n    --hash=sha256:{hashlib.sha256(whl.read_bytes()).hexdigest()}")
PY
} > "$here/requirements-macos-arm64.txt"
echo "wrote $here/requirements-macos-arm64.txt"
