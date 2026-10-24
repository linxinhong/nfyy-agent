// Package dlp provides the baseline outbound DLP scanner: deterministic
// pattern hits over message content. The engine is deliberately simple
// (regex set); the M5 governance milestone extends coverage and rule
// management. The fail-closed contract lives with the caller (llmkernel):
// a scan error blocks the request.
package dlp

import (
	"context"
	"regexp"

	"enterprise-agent-platform/packages/llmkernel"
)

// patterns cover the P0 secret shapes from the PRD DLP list.
var patterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"api_key_openai", regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`)},
	{"api_key_agk", regexp.MustCompile(`agk_(live|test)_[A-Za-z0-9]{16,}`)},
	{"bearer_token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9\-._~+/]{20,}`)},
	{"password_field", regexp.MustCompile(`(?i)(password|passwd|pwd)\s*[:=]\s*\S{4,}`)},
	{"private_key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"db_conn_string", regexp.MustCompile(`(?i)(postgres|postgresql|mysql|mongodb)://[^\s"]+:[^\s"]+@`)},
	{"cn_phone", regexp.MustCompile(`\b1[3-9]\d{9}\b`)},
	{"internal_ipv4", regexp.MustCompile(`\b(10|172\.(1[6-9]|2\d|3[01])|192\.168)\.\d{1,3}\.\d{1,3}\b`)},
}

// Scanner implements llmkernel.DLPEngine.
type Scanner struct{}

// Scan returns the first matching pattern as a block. Content is either
// inspected-clean or blocked; partial redaction ships with M5.
func (Scanner) Scan(_ context.Context, text string) (llmkernel.DLPResult, error) {
	for _, p := range patterns {
		if p.re.MatchString(text) {
			return llmkernel.DLPResult{
				Blocked: true,
				Reason:  "content matches DLP pattern " + p.name,
			}, nil
		}
	}
	return llmkernel.DLPResult{Redacted: text}, nil
}
