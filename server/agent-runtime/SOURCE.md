# Source of this directory

Snapshot of `xingding-agent-runtime` (`github.com/linxinhong/xingding-agent-runtime`) at commit `825e71f` ("merge: agent core hardening for multi-replica servers"), imported into the MindLink center so the model curator (`server/eap/packages/curatoragent`) builds without access to the original repository.

- It stays its own Go module; `server/eap/go.mod` points at it with `replace github.com/linxinhong/xingding-agent-runtime => ../agent-runtime`. Import paths are unchanged.
- Imported: every tracked file except `docs/`, `.tasks/` and `.github/`. Local changes after import: `gofmt -w` (six files).
- Changes are made here and verified with `node server/scripts/eap.mjs runtime` (gofmt, vet, unit tests) plus the curation tests in `server/eap`. The original repository is no longer the source of truth for the center.
