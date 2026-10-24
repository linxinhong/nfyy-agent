package main

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"enterprise-agent-platform/packages/dlp"

	"enterprise-agent-platform/packages/llmkernel"
)

func passThroughScan(_ context.Context, text string) (llmkernel.DLPResult, error) {
	return llmkernel.DLPResult{Redacted: text}, nil
}

// Regression (SSE sanitizer): the sliding-window release must never cut a
// multi-byte UTF-8 rune. The byte-exact release used to emit dangling bytes
// (json.Marshal turns them into U+FFFD) and orphaned the rune tail in the
// window, corrupting CJK output chunk by chunk.
func TestSSESanitizerReleasesAtRuneBoundaries(t *testing.T) {
	san := newSSESanitizer(func(text string) (llmkernel.DLPResult, error) {
		return passThroughScan(context.Background(), text)
	}, 64)
	sentence := "架构评审通过，下一步进入实施阶段。"
	var assembled strings.Builder
	for i := 0; i < 20; i++ {
		out, aborted, err := san.Push(sentence)
		if err != nil || aborted {
			t.Fatalf("push %d: %v %v", i, err, aborted)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("push %d released invalid UTF-8: %q", i, out)
		}
		assembled.WriteString(out)
	}
	tail, aborted, err := san.Flush()
	if err != nil || aborted {
		t.Fatalf("flush: %v %v", err, aborted)
	}
	assembled.WriteString(tail)

	text := assembled.String()
	if want := strings.Repeat(sentence, 20); text != want {
		t.Fatalf("assembled text corrupted:\n got %q\nwant %q", text, want)
	}
}

// The released head must also stay valid when re-substitution moves the cut
// to a '[' pseudonym boundary followed by rune alignment.
func TestSSESanitizerRuneSafeWithRelease(t *testing.T) {
	san := newSSESanitizer(func(text string) (llmkernel.DLPResult, error) {
		return llmkernel.DLPResult{Redacted: text}, nil
	}, 64)
	san.release = func(text string) string { return text }
	sentence := "评审通过，电话是[internal_phone:a1b2c3d4e5]，请查收。"
	var assembled strings.Builder
	for i := 0; i < 20; i++ {
		out, aborted, err := san.Push(sentence)
		if err != nil || aborted {
			t.Fatalf("push %d: %v %v", i, err, aborted)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("push %d released invalid UTF-8: %q", i, out)
		}
		assembled.WriteString(out)
	}
	tail, aborted, err := san.Flush()
	if err != nil || aborted {
		t.Fatalf("flush: %v %v", err, aborted)
	}
	if got := assembled.String() + tail; got != strings.Repeat(sentence, 20) {
		t.Fatalf("assembled text corrupted with release path: %q", got[:120])
	}
}

// TASK-039 regression: the holdback window derives from the live rule set,
// so a dictionary term longer than the old hardcoded 80-byte window can
// never straddle an SSE release boundary. The first chunk must release
// nothing while the term is still completable.
func TestSSESanitizerHoldbackDerivedFromRules(t *testing.T) {
	term := strings.Repeat("机密", 60) // 240-byte dictionary term, multi-byte
	scanner := dlp.NewTableScanner(func(ctx context.Context) ([]dlp.Rule, error) {
		return []dlp.Rule{{Name: "codename", Pattern: `PLACEHOLDER`, Terms: []string{term}, Action: "redact"}}, nil
	})
	maxMatch, bounded, err := scanner.MaxMatchBytes(context.Background())
	if err != nil || !bounded {
		t.Fatalf("bounded rule set must derive a bound: %d %v %v", maxMatch, bounded, err)
	}
	if maxMatch < len(term) {
		t.Fatalf("derived bound %d must cover the %d-byte term", maxMatch, len(term))
	}

	san := newSSESanitizer(func(text string) (llmkernel.DLPResult, error) {
		return scanner.Scan(context.Background(), text)
	}, maxMatch)

	// The old code passed 64 (-> 80-byte holdback): a 100-byte first chunk
	// would have released the term's head before the tail ever arrived.
	first, aborted, err := san.Push(term[:100])
	if err != nil || aborted || first != "" {
		t.Fatalf("derived holdback must keep a completable match in the window: %q %v %v", first, aborted, err)
	}
	rest, aborted, err := san.Push(term[100:])
	if err != nil || aborted {
		t.Fatalf("completion push failed: %v %v", rest, err)
	}
	tail, aborted, err := san.Flush()
	if err != nil || aborted {
		t.Fatalf("flush failed: %v", err)
	}
	assembled := first + rest + tail
	if strings.Contains(assembled, term) {
		t.Fatalf("cross-boundary term must be redacted: %q", assembled)
	}
	if !strings.Contains(assembled, "[REDACTED:codename]") {
		t.Fatalf("redaction must surface: %q", assembled)
	}
}
