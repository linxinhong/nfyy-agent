"""Reproduce the original Hash baseline, independent of later knowledge changes."""
import io
from pathlib import Path
import subprocess
import tarfile
import tempfile

root = Path(__file__).resolve().parents[3]
source = "0c8abee7dcb2b818fcfa8afd62d3294bc6547946"
artifacts = "f16976029f35ffd3a0b380ba37603203aea00a04"
base = "docs/knowledge-memory/eval/"

def git(*args):
    return subprocess.check_output(["git", "-C", str(root), *args])

with tempfile.TemporaryDirectory(prefix="eap-hash-baseline-") as directory:
    out = Path(directory)
    archive = git("archive", source)
    with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
        # Only the fixed, trusted repository commit; reject traversal/symlinks.
        for entry in tar.getmembers():
            assert not entry.name.startswith("/") and ".." not in Path(entry.name).parts
            assert entry.isfile() or entry.isdir(), entry.name
        tar.extractall(out, filter="data")
    (out / base).mkdir(parents=True, exist_ok=True)
    for name in ("hash-baseline.go", "dataset-v1.json"):
        (out / base / name).write_bytes(git("show", artifacts + ":" + base + name))
    result = subprocess.check_output(["go", "run", "./" + base + "hash-baseline.go"], cwd=out)
    expected = git("show", artifacts + ":" + base + "hash-baseline-v1.json")
    assert result == expected, "Frozen baseline reproduction changed"
print("PASS: fixed source " + source + "; artifacts " + artifacts + "; byte-identical output")
