// Package agentdist implements Agent client release distribution
// (TASK-019, ADR-0018): same-domain short-lived download tickets, issuance
// and redemption both re-checking authorization/status, platform matching,
// minimum-version policy, atomic installation with rollback.
package agentdist

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/skillreg"
)

// Release is one listed agent release (no payload, no credentials).
type Release struct {
	Client         string `json:"client"`
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	MinimumVersion string `json:"minimum_version"`
	Notes          string `json:"notes"`
	Digest         string `json:"digest"`
	SignedBy       string `json:"signed_by"`
}

// Catalog is the listing/redemption surface over control data.
type Catalog interface {
	List(ctx context.Context, workspace, client, goos, goarch string) ([]Release, error)
	// TicketIssue mints a short-lived same-domain ticket after re-checking
	// workspace assignment and release status.
	TicketIssue(ctx context.Context, workspace, client, version, goos, goarch, userID string) (string, error)
	// TicketRedeem returns (payload, digest, signatureJSON) after re-checking
	// ticket validity, single redemption, release status and workspace.
	TicketRedeem(ctx context.Context, ticket string) ([]byte, string, []byte, error)
}

// ---------- server-side ticket helpers ----------

// NewTicket mints a cryptographically random ticket id.
func NewTicket() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand unavailable: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// VersionMeet compares dotted numeric versions (major.minor[.patch]).
func VersionMeet(have, minimum string) bool {
	haveN, minN := versionParts(have), versionParts(minimum)
	for i := 0; i < 3; i++ {
		if haveN[i] != minN[i] {
			return haveN[i] > minN[i]
		}
	}
	return true
}

func versionParts(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		fmt.Sscanf(part, "%d", &out[i])
	}
	return out
}

// ---------- install engine with atomic activation ----------

// InstallOptions configures one agent install.
type InstallOptions struct {
	Client      string
	Version     string
	InstallRoot string // owned by the local adapter (e.g. ~/.eap/agents)
	Keys        []skillreg.PkgKey
	GOOS        string
	GOARCH      string
}

// DownloadFunc redeems the ticket and returns bytes + digest + signature.
type DownloadFunc func(ctx context.Context, ticket string) ([]byte, string, []byte, error)

// InstallResult reports one install.
type InstallResult struct {
	Client     string
	Version    string
	Binary     string
	Activated  bool
	RolledBack bool
}

// Install verifies then atomically activates the agent binary, keeping the
// prior installation for rollback on any failure.
func Install(ctx context.Context, c Catalog, o InstallOptions, download DownloadFunc) (InstallResult, error) {
	res := InstallResult{Client: o.Client, Version: o.Version}

	ticket, err := c.TicketIssue(ctx, "", o.Client, o.Version, o.GOOS, o.GOARCH, "")
	if err != nil {
		return res, err
	}
	payload, digest, sigJSON, err := download(ctx, ticket)
	if err != nil {
		return res, err
	}
	// Digest recompute (tamper check) then offline signature verification.
	if skillreg.ArchiveDigest(payload) != digest {
		return res, errs.New(errs.InvalidArgument, "downloaded agent bytes do not match the release digest")
	}
	var sig skillreg.Signature
	if err := json.Unmarshal(sigJSON, &sig); err != nil {
		return res, errs.New(errs.InvalidArgument, "release signature envelope is corrupt")
	}
	if err := skillreg.VerifyRelease(sig, o.Keys); err != nil {
		return res, err
	}

	// Atomic activation: stage -> rename over the prior binary; keep the
	// prior bytes in memory for rollback.
	binaryPath := filepath.Join(o.InstallRoot, o.GOOS+"-"+o.GOARCH, o.Client, agentBinaryName(o.Client, o.GOOS))
	var prior []byte
	priorExisted := false
	if data, err := os.ReadFile(binaryPath); err == nil {
		prior, priorExisted = data, true
	}
	if err := osMkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		return res, errs.Wrap(err, errs.Internal, "install dir create failed")
	}
	staged := binaryPath + ".staged"
	if err := osWriteFile(staged, payload, 0o755); err != nil {
		return res, errs.Wrap(err, errs.Internal, "staging write failed")
	}
	if err := osRename(staged, binaryPath); err != nil {
		return res, errs.Wrap(err, errs.Internal, "activation failed")
	}
	// Activation succeeded only if the bytes on disk match; otherwise roll back.
	onDisk, err := os.ReadFile(binaryPath)
	if err != nil || !bytesEqualGo(onDisk, payload) {
		if priorExisted {
			_ = osWriteFile(binaryPath, prior, 0o755)
		}
		res.RolledBack = true
		return res, errs.New(errs.Internal, "activation verification failed — prior installation restored")
	}
	res.Binary, res.Activated = binaryPath, true
	_ = prior
	return res, nil
}

func agentBinaryName(client, goos string) string {
	if goos == "windows" {
		return client + ".exe"
	}
	return client
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

var (
	osReadFile  = os.ReadFile
	osWriteFile = os.WriteFile
	osRename    = os.Rename
	osMkdirAll  = os.MkdirAll
)

// Command seam: `eap agent list/install`.

func bytesEqualGo(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
