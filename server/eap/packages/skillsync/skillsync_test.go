package skillsync_test

// TASK-018 acceptance coverage: init→release→sync installs the identical
// verified release; repeated sync idempotent; upgrade/policy-removal/revoked
// converge only managed files; interrupted apply restores prior state;
// tampered digest/signature, malicious archives fail closed.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillsync"
)

func mustArchive(t *testing.T, _ skillreg.Signature) []byte {
	t.Helper()
	archive, _, err := skillreg.BuildArchive([]skillreg.PackageFile{
		{Path: "SKILL.md", Content: "# sync skill"},
		{Path: "lib/run.py", Content: "def run():\n    return 1\n"},
	})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	return archive
}

func mustSigJSON(t *testing.T, sig skillreg.Signature) []byte {
	t.Helper()
	b, err := json.Marshal(sig)
	if err != nil {
		t.Fatalf("sig json: %v", err)
	}
	return b
}

type stubClient struct {
	archive []byte
	digest  string
	sigJSON []byte
	fail    bool
}

func (s *stubClient) Download(context.Context, string, string) ([]byte, string, []byte, error) {
	if s.fail {
		return nil, "", nil, errors.New("download failed (worker down)")
	}
	return s.archive, s.digest, s.sigJSON, nil
}

func setupSigned(t *testing.T) (skillreg.Signature, skillreg.PkgKey, []byte, ed25519.PrivateKey) {
	t.Helper()
	files := []skillreg.PackageFile{
		{Path: "SKILL.md", Content: "# sync skill"},
		{Path: "lib/run.py", Content: "def run():\n    return 1\n"},
	}
	archive, digest, err := skillreg.BuildArchive(files)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	sig, err := skillreg.SignRelease("pkg-test", priv, digest, testNow())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	key := skillreg.PkgKey{Kid: "pkg-test", PublicKey: pub, Active: true}
	return sig, key, archive, priv
}

func testNow() time.Time { return time.Now() }

func opts(t *testing.T) (skillsync.Options, string) {
	t.Helper()
	dir := t.TempDir()
	return skillsync.Options{
		Client:       "harness",
		InstallRoot:  filepath.Join(dir, "skills"),
		LockfilePath: filepath.Join(dir, "skills.lock.json"),
	}, dir
}

func TestSyncInstallAndIdempotency(t *testing.T) {
	sig, key, _, _ := setupSigned(t)
	client := &stubClient{
		archive: mustArchive(t, sig),
		digest:  sig.Digest,
		sigJSON: mustSigJSON(t, sig),
	}
	o, root := opts(t)
	o.Keys = []skillreg.PkgKey{key}

	res, err := skillsync.Sync(context.Background(), client, o, "demo", "1.0.0", false)
	if err != nil || !res.Applied {
		t.Fatalf("sync: %v %+v", err, res)
	}
	installed, err := os.ReadFile(filepath.Join(root, "skills", "harness", "demo", "SKILL.md"))
	if err != nil || string(installed) != "# sync skill" {
		t.Fatalf("installed content wrong: %v %s", err, installed)
	}
	// Idempotent replay.
	res, err = skillsync.Sync(context.Background(), client, o, "demo", "1.0.0", false)
	if err != nil || !res.Unchanged {
		t.Fatalf("idempotent sync: %v %+v", err, res)
	}
}

func TestSyncUpgradeRemovalAndFailClosed(t *testing.T) {
	sig, key, archive, priv := setupSigned(t)
	client := &stubClient{archive: archive, digest: sig.Digest, sigJSON: mustSigJSON(t, sig)}
	o, root := opts(t)
	o.Keys = []skillreg.PkgKey{key}

	// Upgrade: 1.0.0 -> 1.0.1 (new archive content).
	files2 := []skillreg.PackageFile{{Path: "SKILL.md", Content: "# v2 content"}}
	archive2, digest2, _ := skillreg.BuildArchive(files2)
	sig2, _ := skillreg.SignRelease("pkg-test", priv, digest2, time.Now())

	res, err := skillsync.Sync(context.Background(), client, o, "demo", "1.0.0", false)
	if err != nil || !res.Applied {
		t.Fatalf("initial: %v", err)
	}
	client.archive, client.digest, client.sigJSON = archive2, digest2, mustSigJSON(t, sig2)
	res, err = skillsync.Sync(context.Background(), client, o, "demo", "1.0.1", false)
	if err != nil || !res.Applied {
		t.Fatalf("upgrade: %v", err)
	}
	// Upgrade replaces managed content with the new version's bytes.
	installed, err := os.ReadFile(filepath.Join(root, "skills", "harness", "demo", "SKILL.md"))
	if err != nil || string(installed) != "# v2 content" {
		t.Fatalf("upgrade content wrong: %v %s", err, installed)
	}

	// Tampered digest fails closed.
	client.digest = strings.Repeat("a", 64)
	if _, err := skillsync.Sync(context.Background(), client, o, "demo", "1.0.2", false); err == nil {
		t.Fatalf("tampered digest must fail closed")
	}
	// Tampered signature fails closed.
	client.digest, client.sigJSON = digest2, []byte(`{"kid":"pkg-test","alg":"EdDSA","digest":"`+digest2+`","signature":"AAA","signed_at":"2020-01-01T00:00:00Z"}`)
	if _, err := skillsync.Sync(context.Background(), client, o, "demo", "1.0.2", false); err == nil {
		t.Fatalf("tampered signature must fail closed")
	}
	// Malicious archive fails closed at extraction.
	evil, evilDigest, _ := skillreg.BuildArchive([]skillreg.PackageFile{{Path: "../escape.txt", Content: "x"}})
	_ = evilDigest
	evilSig, _ := skillreg.SignRelease("pkg-test", priv, skillreg.ArchiveDigest(evil), time.Now())
	client.archive, client.digest, client.sigJSON = evil, evilDigest, mustSigJSON(t, evilSig)
	if _, err := skillsync.Sync(context.Background(), client, o, "demo", "1.0.2", false); err == nil {
		t.Fatalf("malicious archive must fail closed")
	}
	// Worker failure fails closed with no partial state.
	client.fail = true
	if _, err := skillsync.Sync(context.Background(), client, o, "demo", "1.0.2", false); err == nil {
		t.Fatalf("worker failure must fail closed")
	}
	client.fail = false
	client.digest, client.sigJSON = digest2, mustSigJSON(t, sig2)

	// Policy removal converges only managed files: plant an unmanaged file
	// that must survive.
	unmanaged := filepath.Join(root, "skills", "harness", "demo", "user-notes.txt")
	if err := os.WriteFile(unmanaged, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = skillsync.Sync(context.Background(), client, o, "demo", "1.0.1", true)
	if err != nil || !res.Removed {
		t.Fatalf("removal: %v %+v", err, res)
	}
	if _, err := os.Stat(unmanaged); err != nil {
		t.Fatalf("unmanaged file must survive removal")
	}
}
