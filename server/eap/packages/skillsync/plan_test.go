package skillsync_test

// TASK-018 desired-state planning coverage: plan diffs the workspace desired
// state against the lockfile and catalog; apply converges only managed files;
// the lockfile records the verifying package key id; version ordering is
// numeric-segment aware.

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillsync"
)

type stubCatalog struct {
	rows []skillsync.CatalogEntry
}

func (s *stubCatalog) Catalog(context.Context) ([]skillsync.CatalogEntry, error) {
	return s.rows, nil
}

func signVersion(t *testing.T, priv ed25519.PrivateKey, content string) (skillreg.Signature, []byte) {
	t.Helper()
	archive, digest, err := skillreg.BuildArchive([]skillreg.PackageFile{{Path: "SKILL.md", Content: content}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sig, err := skillreg.SignRelease("pkg-plan", priv, digest, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return sig, archive
}

func TestCompareVersionsNumericSegments(t *testing.T) {
	if skillsync.CompareVersions("0.10.0", "0.9.0") <= 0 {
		t.Fatalf("0.10.0 must sort above 0.9.0")
	}
	if skillsync.CompareVersions("1.0.0", "1.0.0") != 0 {
		t.Fatalf("equal versions must compare 0")
	}
}

func TestPlanInstallUpgradeUnchangedAndRemove(t *testing.T) {
	_, key, _, priv := setupSigned(t)
	sig, signedArchive := signVersion(t, priv, "# plan content")
	client := &stubClient{archive: signedArchive, digest: sig.Digest, sigJSON: mustSigJSON(t, sig)}
	catalog := &stubCatalog{rows: []skillsync.CatalogEntry{
		{Name: "alpha", Version: "0.9.0", Status: "released"},
		{Name: "alpha", Version: "0.10.0", Status: "released"},
		{Name: "alpha", Version: "0.11.0", Status: "deprecated"},
		{Name: "beta", Version: "1.0.0", Status: "pending_review"},
		{Name: "gamma", Version: "2.0.0", Status: "released"},
	}}

	o, _ := opts(t)
	key.Kid = "pkg-plan"
	o.Keys = []skillreg.PkgKey{key}
	lock := o.LockfilePath
	desired := skillsync.DesiredState{
		Skills:  []skillsync.DesiredSkill{{Name: "alpha"}, {Name: "gamma", Version: "2.0.0"}},
		Clients: []string{"harness", "codex"},
	}

	// First plan: install alpha@latest-released (0.10.0, not the deprecated
	// 0.11.0) for both clients plus gamma@2.0.0.
	plan, err := skillsync.Plan(context.Background(), catalog, skillsync.Options{LockfilePath: lock}, desired)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	seen := map[string]skillsync.PlanAction{}
	for _, a := range plan {
		seen[a.Client+"/"+a.Name] = a
	}
	if a := seen["harness/alpha"]; a.Action != skillsync.ActionInstall || a.To != "0.10.0" {
		t.Fatalf("alpha plan: %+v", a)
	}
	if a := seen["codex/gamma"]; a.Action != skillsync.ActionInstall || a.To != "2.0.0" {
		t.Fatalf("gamma plan: %+v", a)
	}
	if len(plan) != 4 {
		t.Fatalf("expected 4 install actions, got %+v", plan)
	}

	// Apply for harness only; codex stays untouched.
	results, err := skillsync.ApplyPlan(context.Background(), client, o, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("harness results: %+v", results)
	}
	lockData, err := skillsync.LoadLockfile(lock)
	if err != nil {
		t.Fatalf("lockfile: %v", err)
	}
	var alphaEntry *skillsync.LockEntry
	for i := range lockData.Entries {
		if lockData.Entries[i].Name == "alpha" {
			alphaEntry = &lockData.Entries[i]
		}
	}
	if alphaEntry == nil || alphaEntry.KeyID != "pkg-plan" {
		t.Fatalf("lockfile must record the verifying key id: %+v", alphaEntry)
	}

	// Re-plan: alpha unchanged for harness, still install for codex.
	plan2, err := skillsync.Plan(context.Background(), catalog, skillsync.Options{LockfilePath: lock}, desired)
	if err != nil {
		t.Fatalf("plan2: %v", err)
	}
	for _, a := range plan2 {
		if a.Client == "harness" && a.Name == "alpha" && a.Action != skillsync.ActionUnchanged {
			t.Fatalf("alpha must be unchanged after apply: %+v", a)
		}
	}

	// Simulate an old codex install that desired state no longer includes:
	// planning must converge it to remove for that client only.
	codexOpts := o
	codexOpts.Client = "codex"
	if _, err := skillsync.Sync(context.Background(), client, codexOpts, "legacy", "0.1.0", false); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	plan3, err := skillsync.Plan(context.Background(), catalog, skillsync.Options{LockfilePath: lock}, desired)
	if err != nil {
		t.Fatalf("plan3: %v", err)
	}
	var legacyRemoval bool
	for _, a := range plan3 {
		if a.Client == "codex" && a.Name == "legacy" {
			legacyRemoval = a.Action == skillsync.ActionRemove
		}
	}
	if !legacyRemoval {
		t.Fatalf("legacy entry must plan a remove: %+v", plan3)
	}
	if _, err := skillsync.ApplyPlan(context.Background(), client, codexOpts, plan3); err != nil {
		t.Fatalf("apply codex: %v", err)
	}
	lockData2, _ := skillsync.LoadLockfile(lock)
	for _, e := range lockData2.Entries {
		if e.Client == "codex" && e.Name == "legacy" {
			t.Fatalf("legacy entry must be gone: %+v", e)
		}
	}

	// A specified desired version that the catalog never released fails
	// closed instead of converging.
	bad := skillsync.DesiredState{
		Skills:  []skillsync.DesiredSkill{{Name: "beta", Version: "1.0.0"}},
		Clients: []string{"harness"},
	}
	if _, err := skillsync.Plan(context.Background(), catalog, skillsync.Options{LockfilePath: lock}, bad); err == nil {
		t.Fatalf("unreleased specified version must fail closed")
	}
}
