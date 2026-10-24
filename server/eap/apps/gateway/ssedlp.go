package main

// SSE cross-chunk DLP sanitizer (TASK-030): streaming chunks are released
// only up to the prefix that can no longer match any rule; the tail rides a
// bounded sliding window (longest candidate length + margin) and the final
// flush scans everything. A deny hit aborts the stream; tokenize/redact
// rewrite the released text. Streaming liveness is preserved — we never
// buffer the whole response.

import (
	"strings"
	"unicode/utf8"

	"enterprise-agent-platform/packages/llmkernel"
)

// sseSanitizer is the per-stream sliding-window state machine.
type sseSanitizer struct {
	scan   func(text string) (llmkernel.DLPResult, error)
	window strings.Builder
	// holdback is the maximum bytes a rule match can span (longest pattern
	// + separator margin); content beyond it can be released safely.
	holdback int
	// release, when set, transforms text on its way out (e.g. exempt-role
	// vault re-substitution). Failures fail closed to the pseudonym form.
	release func(text string) string
}

const sseHoldbackMargin = 16

func newSSESanitizer(scan func(string) (llmkernel.DLPResult, error), maxMatch int) *sseSanitizer {
	hold := maxMatch + sseHoldbackMargin
	if hold < 32 {
		hold = 32
	}
	return &sseSanitizer{scan: scan, holdback: hold}
}

// Push adds a chunk and returns the bytes safe to emit plus an aborted flag
// (deny hit). The window tail is retained for cross-boundary matches.
func (s *sseSanitizer) Push(delta string) (string, bool, error) {
	s.window.WriteString(delta)
	full := s.window.String()
	res, err := s.scan(full)
	if err != nil {
		return "", false, err
	}
	if res.Blocked {
		return "", true, nil
	}
	clean := res.Redacted
	if len(clean) <= s.holdback {
		// Everything stays in the window until more context arrives.
		s.window.Reset()
		s.window.WriteString(clean)
		return "", false, nil
	}
	cut := len(clean) - s.holdback
	if s.release != nil {
		// Re-substitution works per released chunk; an unterminated '['
		// pseudonym prefix at the cut would split a token across releases.
		// Hold back from the last '[' that has no ']' after it (bounded by
		// the max pseudonym length, so the window stays bounded).
		head := clean[:cut]
		lastOpen := strings.LastIndexByte(head, '[')
		if lastOpen > strings.LastIndexByte(head, ']') && cut-lastOpen < 128 {
			cut = lastOpen
		}
	}
	// Never split a UTF-8 rune at the release boundary: a mid-rune cut emits
	// dangling bytes (json.Marshal turns them into U+FFFD) and orphans the
	// rest in the window, corrupting every multi-byte character that lands
	// on the cut. Back up to the nearest rune start; the held-back tail is
	// at most two bytes larger and stays bounded.
	for cut > 0 && !utf8.RuneStart(clean[cut]) {
		cut--
	}
	out := clean[:cut]
	s.window.Reset()
	s.window.WriteString(clean[cut:])
	if s.release != nil {
		out = s.release(out)
	}
	return out, false, nil
}

// Flush scans the remaining window at stream end and returns the tail.
func (s *sseSanitizer) Flush() (string, bool, error) {
	full := s.window.String()
	s.window.Reset()
	if full == "" {
		return "", false, nil
	}
	res, err := s.scan(full)
	if err != nil {
		return "", false, err
	}
	if res.Blocked {
		return "", true, nil
	}
	out := res.Redacted
	if s.release != nil {
		out = s.release(out)
	}
	return out, false, nil
}
