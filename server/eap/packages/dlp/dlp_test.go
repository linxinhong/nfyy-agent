package dlp_test

import (
	"context"
	"testing"

	"enterprise-agent-platform/packages/dlp"
)

func TestScannerBlocksSecretShapes(t *testing.T) {
	s := dlp.Scanner{}
	blocked := []string{
		"my key is sk-abcdefghij0123456789AB ok",
		"use agk_live_abcdefghijklmnop12 for access",
		"Authorization: Bearer abcdefghij0123456789",
		"password: hunter22",
		"-----BEGIN RSA PRIVATE KEY-----",
		"postgres://admin:hunter22@db.internal:5432/eap",
		"call me at 13812345678",
		"server at 10.1.2.3",
	}
	for _, text := range blocked {
		result, err := s.Scan(context.Background(), text)
		if err != nil {
			t.Fatalf("scan(%q): %v", text, err)
		}
		if !result.Blocked {
			t.Errorf("secret shape not blocked: %q", text)
		}
	}
}

func TestScannerAllowsCleanContent(t *testing.T) {
	s := dlp.Scanner{}
	result, err := s.Scan(context.Background(), "summarize the failing test in workspace secops")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if result.Blocked {
		t.Fatalf("clean content blocked: %+v", result)
	}
	if result.Redacted != "summarize the failing test in workspace secops" {
		t.Fatalf("clean content must pass through unchanged")
	}
}
