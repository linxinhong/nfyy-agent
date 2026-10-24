//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"enterprise-agent-platform/packages/llmkernel"
)

// sseThroughputForTest drives the real sliding-window sanitizer the stream
// handler uses and reports chunks/sec.
func SSEThroughputForTest(t *testing.T, engine llmkernel.DLPEngine) int {
	t.Helper()
	san := newSSESanitizer(func(text string) (llmkernel.DLPResult, error) {
		return engine.Scan(context.Background(), text)
	}, 64)
	chunk := "架构评审通过，下一步进入实施阶段。"
	n := 500
	t0 := time.Now()
	for i := 0; i < n; i++ {
		if _, aborted, err := san.Push(chunk); err != nil || aborted {
			t.Fatalf("push %d: %v %v", i, err, aborted)
		}
	}
	if _, aborted, err := san.Flush(); err != nil || aborted {
		t.Fatalf("flush: %v %v", err, aborted)
	}
	elapsed := time.Since(t0).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return int(float64(n) / elapsed)
}
