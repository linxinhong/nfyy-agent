"""Check frozen TASK-056 artifacts; does not validate runtime security or quality."""
import hashlib
import json
import math
from pathlib import Path
import re

root = Path(__file__).resolve().parents[3]
eval_dir = root / "docs/knowledge-memory/eval"
data_bytes = (eval_dir / "dataset-v1.json").read_bytes()
dataset = json.loads(data_bytes)
baseline = json.loads((eval_dir / "hash-baseline-v1.json").read_text())
assert hashlib.sha256(data_bytes).hexdigest() == baseline["dataset_sha256"]
assert baseline["dataset_sha256"] in (eval_dir / "README.md").read_text()
docs = {d["id"]: d for d in dataset["documents"]}
queries = {q["id"]: q for q in dataset["queries"]}
assert len(docs) == len(dataset["documents"]) == 10
assert len(queries) == len(dataset["queries"]) == 12
assert {q["group"] for q in queries.values()} == {
    "exact", "chinese", "paraphrase", "no_result", "workspace_trap"
}
assert {r["query"] for r in baseline["results"]} == set(queries)
for q in queries.values():
    assert all(docs[i]["workspace"] == q["workspace"] for i in q["relevant"])
    assert all(g in (1, 2) for g in q["relevant"].values())
for result in baseline["results"]:
    q = queries[result["query"]]
    assert result["group"] == q["group"]
    assert len(result["hits"]) <= dataset["top_k"]
    assert all(docs[h["id"]]["workspace"] == q["workspace"] for h in result["hits"])
    assert all(math.isfinite(h["score"]) and h["score"] > 0 for h in result["hits"])
    assert result["empty"] == (not result["hits"])
fixtures = root / "contracts/schemas/v1/enterprise"
examples = {p.name: json.loads(p.read_text()) for p in fixtures.glob("knowledge.*.json")}
assert len(examples) == 6
upload = examples["knowledge.upload.response.json"]
assert set(upload) == {"document_id", "title", "version", "status", "idempotent_replay", "redactions"}
for name in ("knowledge.search.response.json", "knowledge.derived-search.response.json"):
    for hit in examples[name]["results"]:
        assert {"document_id", "title", "version", "seq", "score", "content"} <= hit.keys()
        assert hit["score_kind"] == "rrf" and 0 < hit["score"] <= 2 / 61
        assert hit["version"] >= 1 and hit["seq"] >= 0
        assert bool(hit["source"]["parents"]) == hit["source"]["derived"]
parent = examples["knowledge.derived-search.response.json"]["results"][0]["source"]["parents"][0]
text = examples["knowledge.upload.request.json"]["content"]
assert parent["locator"] == {"kind": "text_range", "start": 0, "end": len(text)}
assert parent["digest"] == "sha256:" + hashlib.sha256(text.encode()).hexdigest()
assert examples["knowledge.empty-search.response.json"] == {"results": []}
# Relative Markdown links in new contract docs, not historical external checkout links.
paths = [root / "docs/adr/0029-knowledge-retrieval-generations.md"]
paths += [root / "docs/knowledge-memory" / n for n in ("contracts.md", "reuse-and-handoff.md", "eval/README.md")]
for path in paths:
    for target in re.findall(r"\]\(([^)]+)\)", path.read_text()):
        if not target.startswith(("https://", "http://", "#")):
            assert (path.parent / target.split("#")[0]).exists(), (path, target)
print("PASS: dataset digest, labels, baseline shape, six compatibility fixtures, provenance digest/range, contract links")
