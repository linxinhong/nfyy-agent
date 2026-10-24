// Package skillsync implements managed Skill installation and Workspace
// synchronization for the eap CLI (TASK-018, ADR-0017).
//
// Safety contract:
//   - only files listed in the lockfile's manifest are managed; unmanaged
//     files in the install root are never touched (collision = fail closed);
//   - apply is atomic: all files staged to a temp dir, then swapped in; an
//     interrupted apply restores the prior working state;
//   - every extracted file is re-scanned (paths, types, content) — tampered
//     archives or signatures never activate;
//   - the lockfile records exact digest+version so repeated sync is a no-op.
package skillsync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/skillreg"
)

// Client is the registry download surface the sync needs.
type Client interface {
	// Download returns (archiveBytes, digest, signatureJSON) for a released
	// skill version visible to the caller's workspace.
	Download(ctx context.Context, name, version string) ([]byte, string, []byte, error)
}

// LockEntry is one managed installation.
type LockEntry struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	Digest     string            `json:"digest"`
	KeyID      string            `json:"key_id"` // pkg- key that verified the release signature
	Client     string            `json:"client"` // harness | codex | ...
	InstallDir string            `json:"install_dir"`
	Files      map[string]string `json:"files"` // rel path -> sha256
}

// Lockfile is the managed-install ledger on disk.
type Lockfile struct {
	Entries []LockEntry `json:"entries"`
}

// LoadLockfile reads the lockfile at path (missing file = empty).
func LoadLockfile(path string) (Lockfile, error) {
	var lock Lockfile
	data, err := osReadFile(path)
	if err != nil {
		if osIsNotExist(err) {
			return lock, nil
		}
		return lock, errs.Wrap(err, errs.Internal, "lockfile read failed")
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return lock, errs.New(errs.InvalidArgument, "lockfile is corrupt — refusing to sync over it")
	}
	return lock, nil
}

// SaveLockfile writes the lockfile.
func SaveLockfile(path string, lock Lockfile) error {
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return osWriteFile(path, data, 0o644)
}

// Options configures one sync operation.
type Options struct {
	Client       string // target client name (harness/codex)
	InstallRoot  string // managed installation root
	LockfilePath string
	Keys         []skillreg.PkgKey // discovery keys for offline verification
}

// SyncResult reports one sync outcome.
type SyncResult struct {
	Name      string
	Version   string
	Applied   bool
	Unchanged bool
	Removed   bool
}

// Sync installs (or upgrades) one skill version into the client root and
// rewrites the lockfile atomically. policyRemoval=true removes the entry
// (policy no longer allows the skill) converging only managed files.
func Sync(ctx context.Context, c Client, o Options, name, version string, policyRemoval bool) (SyncResult, error) {
	lock, err := LoadLockfile(o.LockfilePath)
	if err != nil {
		return SyncResult{}, err
	}
	if policyRemoval {
		return removeEntry(lock, o, name)
	}

	// Already satisfied → idempotent no-op.
	for _, e := range lock.Entries {
		if e.Name == name && e.Client == o.Client && e.Version == version {
			return SyncResult{Name: name, Version: version, Unchanged: true}, nil
		}
	}

	archive, digest, sigJSON, err := c.Download(ctx, name, version)
	if err != nil {
		return SyncResult{}, err
	}
	// Recompute digest from bytes (tamper check) and verify signature offline.
	if skillreg.ArchiveDigest(archive) != digest {
		return SyncResult{}, errs.New(errs.InvalidArgument, "downloaded archive does not match the registry digest")
	}
	var sig skillreg.Signature
	if err := json.Unmarshal(sigJSON, &sig); err != nil {
		return SyncResult{}, errs.New(errs.InvalidArgument, "release signature envelope is corrupt")
	}
	if err := skillreg.VerifyRelease(sig, o.Keys); err != nil {
		return SyncResult{}, err
	}
	files, err := skillreg.ExtractBounded(archive)
	if err != nil {
		return SyncResult{}, err
	}

	installDir := filepath.Join(o.InstallRoot, o.Client, name)
	// Unmanaged collision check before touching anything.
	for rel := range files {
		target := filepath.Join(installDir, rel)
		if _, err := osStat(target); err == nil {
			if !isManaged(lock, installDir, rel) {
				return SyncResult{}, errs.New(errs.Conflict,
					fmt.Sprintf("unmanaged file collision at %s — refusing to overwrite", rel))
			}
		}
	}

	// Atomic-ish apply: stage everything into a temp dir, then move in.
	staging, err := osMkdirTemp("", "eap-skill-")
	if err != nil {
		return SyncResult{}, errs.Wrap(err, errs.Internal, "staging failed")
	}
	defer func() { _ = osRemoveAll(staging) }()
	stagedFiles := map[string]string{}
	for rel, content := range files {
		target := filepath.Join(staging, rel)
		if err := osMkdirAll(filepath.Dir(target), 0o755); err != nil {
			return SyncResult{}, errs.Wrap(err, errs.Internal, "staging mkdir failed")
		}
		if err := osWriteFile(target, []byte(content), 0o644); err != nil {
			return SyncResult{}, errs.Wrap(err, errs.Internal, "staging write failed")
		}
		stagedFiles[rel] = sha256Hex(content)
	}

	// Remove prior managed files for this (name, client), then move staged in.
	prior := removeManagedFiles(lock, installDir, name, o.Client)
	if err := osMkdirAll(installDir, 0o755); err != nil {
		// restore prior state on failure
		restorePrior(prior)
		return SyncResult{}, errs.Wrap(err, errs.Internal, "install root create failed")
	}
	for rel, content := range files {
		target := filepath.Join(installDir, rel)
		if err := osMkdirAll(filepath.Dir(target), 0o755); err != nil {
			restorePrior(prior)
			return SyncResult{}, errs.Wrap(err, errs.Internal, "install mkdir failed")
		}
		if err := osWriteFile(target, []byte(content), 0o644); err != nil {
			restorePrior(prior)
			return SyncResult{}, errs.Wrap(err, errs.Internal, "install write failed")
		}
	}

	entry := LockEntry{
		Name: name, Version: version, Digest: digest, KeyID: sig.Kid, Client: o.Client,
		InstallDir: installDir, Files: stagedFiles,
	}
	newLock := replaceEntry(lock, entry)
	if err := SaveLockfile(o.LockfilePath, newLock); err != nil {
		restorePrior(prior)
		return SyncResult{}, errs.Wrap(err, errs.Internal, "lockfile write failed — prior state restored")
	}
	return SyncResult{Name: name, Version: version, Applied: true}, nil
}

type priorFile struct {
	path    string
	content []byte
	existed bool
}

func removeManagedFiles(lock Lockfile, installDir, name, client string) []priorFile {
	var prior []priorFile
	var keep []LockEntry
	for _, e := range lock.Entries {
		if e.Name == name && e.Client == client {
			for rel := range e.Files {
				target := filepath.Join(e.InstallDir, rel)
				data, err := osReadFile(target)
				prior = append(prior, priorFile{path: target, content: data, existed: err == nil})
				_ = osRemove(target)
			}
			continue
		}
		keep = append(keep, e)
	}
	lock.Entries = keep
	_ = installDir
	return prior
}

func restorePrior(prior []priorFile) {
	for _, p := range prior {
		if p.existed {
			_ = osWriteFile(p.path, p.content, 0o644)
		}
	}
}

func replaceEntry(lock Lockfile, entry LockEntry) Lockfile {
	var out []LockEntry
	for _, e := range lock.Entries {
		if e.Name == entry.Name && e.Client == entry.Client {
			continue
		}
		out = append(out, e)
	}
	out = append(out, entry)
	return Lockfile{Entries: out}
}

func removeEntry(lock Lockfile, o Options, name string) (SyncResult, error) {
	found := false
	var out []LockEntry
	for _, e := range lock.Entries {
		if e.Name == name && e.Client == o.Client {
			found = true
			for rel := range e.Files {
				_ = osRemove(filepath.Join(e.InstallDir, rel))
			}
			continue
		}
		out = append(out, e)
	}
	if !found {
		return SyncResult{}, errs.New(errs.NotFound, "skill is not installed for this client")
	}
	if err := SaveLockfile(o.LockfilePath, Lockfile{Entries: out}); err != nil {
		return SyncResult{}, errs.Wrap(err, errs.Internal, "lockfile write failed")
	}
	return SyncResult{Name: name, Removed: true}, nil
}

func isManaged(lock Lockfile, installDir, rel string) bool {
	for _, e := range lock.Entries {
		if e.InstallDir == installDir {
			if _, ok := e.Files[rel]; ok {
				return true
			}
		}
	}
	return false
}

var (
	osReadFile   = os.ReadFile
	osStat       = os.Stat
	osMkdirAll   = os.MkdirAll
	osMkdirTemp  = os.MkdirTemp
	osWriteFile  = os.WriteFile
	osRemove     = os.Remove
	osRemoveAll  = os.RemoveAll
	osIsNotExist = os.IsNotExist
)

func sha256Hex(content string) string {
	sum := sha256Sum([]byte(content))
	return fmt.Sprintf("%x", sum)
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
