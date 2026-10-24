// Package skillreg implements the governed Skill Registry supply chain
// (TASK-016, ADR-0015): deterministic tar.gz packaging, server-side security
// scans, immutable name@version lifecycle under central roles, and
// platform-signed releases with offline-verifiable package keys.
//
// Hard rules:
//   - server validation is authoritative and repeats security checks
//     regardless of any client-declared result;
//   - name@version is immutable — duplicates are rejected;
//   - release requires review AND release authorization; direct-to-stable
//     publishing is impossible in the lifecycle;
//   - package signing keys (kid prefix "pkg-") are purpose-separated from
//     internal JWT keys; rotation keeps historical signatures verifiable;
//   - every transition audits through the reliable path; raw file contents
//     never enter audit details.
package skillreg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"enterprise-agent-platform/packages/errs"
)

// Lifecycle statuses.
const (
	StatusPendingReview = "pending_review"
	StatusApproved      = "approved"
	StatusReleased      = "released"
	StatusDeprecated    = "deprecated"
	StatusRevoked       = "revoked"
)

// Limits bound memory/CPU/storage per package.
const (
	MaxPackageBytes = 1 << 20 // 1 MiB compressed cap
	MaxFiles        = 128
	MaxFileBytes    = 512 << 10
)

// PkgKey is one release signing key (purpose-separated namespace).
type PkgKey struct {
	Kid       string // must start with "pkg-"
	PublicKey ed25519.PublicKey
	Active    bool
	CreatedAt time.Time
	RetiredAt *time.Time
}

// PackageKeyJWK is the public discovery shape.
type PackageKeyJWK struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// KeySetJWK renders the discovery document (active + historical keys so
// offline verification of allowed historical releases keeps working).
func KeySetJWK(keys []PkgKey) map[string]any {
	out := make([]PackageKeyJWK, 0, len(keys))
	for _, k := range keys {
		out = append(out, PackageKeyJWK{
			Kid: k.Kid, Kty: "OKP", Crv: "Ed25519",
			X:   base64.RawURLEncoding.EncodeToString(k.PublicKey),
			Alg: "EdDSA", Use: "sig",
		})
	}
	return map[string]any{"keys": out}
}

// PackageFile is one entry of a publish request.
type PackageFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// BuildArchive produces the deterministic tar.gz for the package: files
// sorted by path, fixed mtimes, no pax gaps — same inputs always produce the
// same bytes and digest.
func BuildArchive(files []PackageFile) ([]byte, string, error) {
	if len(files) == 0 {
		return nil, "", errs.New(errs.InvalidArgument, "package needs at least one file")
	}
	if len(files) > MaxFiles {
		return nil, "", errs.New(errs.InvalidArgument, "package exceeds the file count limit")
	}
	sorted := make([]PackageFile, len(files))
	copy(sorted, files)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	// Security validation before any byte is packaged.
	for _, f := range sorted {
		if err := validatePath(f.Path); err != nil {
			return nil, "", err
		}
		if len(f.Content) > MaxFileBytes {
			return nil, "", errs.New(errs.InvalidArgument, "file exceeds the size limit: "+f.Path)
		}
		if err := ScanContent(f.Path, f.Content); err != nil {
			return nil, "", err
		}
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	// Fixed modification time for determinism.
	epoch := time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	for _, f := range sorted {
		hdr := &tar.Header{
			Name:     f.Path,
			Mode:     0o644,
			Size:     int64(len(f.Content)),
			ModTime:  epoch,
			Typeflag: tar.TypeReg,
			Uname:    "eap", Gname: "eap",
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", errs.Wrap(err, errs.Internal, "tar header failed")
		}
		if _, err := tw.Write([]byte(f.Content)); err != nil {
			return nil, "", errs.Wrap(err, errs.Internal, "tar write failed")
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", errs.Wrap(err, errs.Internal, "tar close failed")
	}
	if err := gz.Close(); err != nil {
		return nil, "", errs.Wrap(err, errs.Internal, "gzip close failed")
	}
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	return archive, hex.EncodeToString(sum[:]), nil
}

// validatePath rejects absolute paths, traversal, backslashes and anything
// that would escape the extraction root. Symlinks cannot occur: the archive
// contract contains only regular files.
func validatePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return errs.New(errs.InvalidArgument, "package path must be relative: "+p)
	}
	clean := path.Clean(p)
	if clean == "." || strings.HasPrefix(clean, "..") || strings.Contains(clean, "../") {
		return errs.New(errs.InvalidArgument, "package path escapes the package root: "+p)
	}
	return nil
}

// scanPatterns are server-authoritative content scans (scripts/secrets).
var scanPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"shell_script", regexp.MustCompile(`(?m)^\s*#!\s*/(bin|usr/bin)/(ba)?sh`)},
	{"eval_invoke", regexp.MustCompile(`(?i)\beval\s*\(`)},
	{"curl_pipe_sh", regexp.MustCompile(`(?i)curl\s+[^\n]*\|\s*(ba)?sh`)},
	{"openai_key", regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`)},
	{"eap_api_key", regexp.MustCompile(`agk_(live|test)_[A-Za-z0-9]{16,}`)},
	{"private_key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
}

// ScanContent is the server-side secret/script scan. A finding is a publish
// rejection; the matched text never leaves this function.
func ScanContent(pathName, content string) error {
	for _, p := range scanPatterns {
		if p.re.MatchString(content) {
			return errs.New(errs.InvalidArgument,
				fmt.Sprintf("scan finding %q in %s — package rejected", p.name, pathName))
		}
	}
	return nil
}

// Signature is the detached release signature envelope.
type Signature struct {
	Kid       string    `json:"kid"`
	Alg       string    `json:"alg"`
	Digest    string    `json:"digest"`
	Signature string    `json:"signature"` // base64(Ed25519(digest bytes))
	SignedAt  time.Time `json:"signed_at"`
}

// SignRelease signs the archive digest with a package key. kid MUST be in
// the pkg- namespace — internal JWT keys are never accepted here.
func SignRelease(kid string, key ed25519.PrivateKey, digest string, now time.Time) (Signature, error) {
	if !strings.HasPrefix(kid, "pkg-") {
		return Signature{}, errors.New("release signing requires a pkg- namespace key")
	}
	raw, err := hex.DecodeString(digest)
	if err != nil {
		return Signature{}, errs.New(errs.Internal, "digest must be hex")
	}
	sig := ed25519.Sign(key, raw)
	return Signature{
		Kid: kid, Alg: "EdDSA", Digest: digest,
		Signature: base64.RawURLEncoding.EncodeToString(sig),
		SignedAt:  now.UTC(),
	}, nil
}

// VerifyRelease verifies a release signature offline against a key set.
func VerifyRelease(sig Signature, keys []PkgKey) error {
	if sig.Kid == "" || !strings.HasPrefix(sig.Kid, "pkg-") {
		return errs.New(errs.InvalidArgument, "signature kid is not a package key")
	}
	raw, err := hex.DecodeString(sig.Digest)
	if err != nil {
		return errs.New(errs.InvalidArgument, "digest must be hex")
	}
	b64, err := base64.RawURLEncoding.DecodeString(sig.Signature)
	if err != nil {
		return errs.New(errs.InvalidArgument, "signature is not base64")
	}
	for _, k := range keys {
		if k.Kid != sig.Kid {
			continue
		}
		if ed25519.Verify(k.PublicKey, raw, b64) {
			return nil
		}
		return errs.New(errs.InvalidArgument, "release signature does not verify against key "+sig.Kid)
	}
	return errs.New(errs.NotFound, "signing key "+sig.Kid+" is unknown")
}

// ArchiveDigest recomputes the sha256 of downloaded bytes (offline check).
func ArchiveDigest(archive []byte) string {
	sum := sha256.Sum256(archive)
	return hex.EncodeToString(sum[:])
}

// ExtractBounded performs a bounded, security-checked extraction of the
// archive into memory: only regular files, paths re-validated, size caps
// enforced, no symlinks/hardlinks/devices.
func ExtractBounded(archive []byte) (map[string]string, error) {
	if len(archive) > MaxPackageBytes {
		return nil, errs.New(errs.InvalidArgument, "archive exceeds the size limit")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, errs.New(errs.InvalidArgument, "archive is not gzip")
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errs.New(errs.InvalidArgument, "archive is corrupt")
		}
		count++
		if count > MaxFiles {
			return nil, errs.New(errs.InvalidArgument, "archive exceeds the file count limit")
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, errs.New(errs.InvalidArgument, "archive contains a non-regular entry: "+hdr.Name)
		}
		if err := validatePath(hdr.Name); err != nil {
			return nil, err
		}
		if hdr.Size > MaxFileBytes {
			return nil, errs.New(errs.InvalidArgument, "entry exceeds the size limit: "+hdr.Name)
		}
		content, err := io.ReadAll(io.LimitReader(tr, MaxFileBytes+1))
		if err != nil {
			return nil, errs.New(errs.InvalidArgument, "entry read failed: "+hdr.Name)
		}
		if int64(len(content)) > MaxFileBytes {
			return nil, errs.New(errs.InvalidArgument, "entry exceeds the size limit: "+hdr.Name)
		}
		if err := ScanContent(hdr.Name, string(content)); err != nil {
			return nil, err
		}
		out[hdr.Name] = string(content)
	}
	if len(out) == 0 {
		return nil, errs.New(errs.InvalidArgument, "archive is empty")
	}
	return out, nil
}
