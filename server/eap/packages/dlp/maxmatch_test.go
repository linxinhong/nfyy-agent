package dlp

import (
	"context"
	"strings"
	"testing"
)

func TestRuleMaxMatch(t *testing.T) {
	cases := []struct {
		pattern string
		want    int
		bounded bool
	}{
		{"138[0-9]{8}", 11, true},
		{`sk-[A-Za-z0-9]{20,64}`, 67, true},
		{"(?:110|119|120)", 3, true},
		{`sk-[A-Za-z0-9]+`, 0, false},  // unbounded repetition
		{`secret-*`, 0, false},         // star over non-empty body
		{"[0-9]{17}[0-9Xx]", 18, true}, // CN ID shape
		// TASK-054: the widened seeded patterns must stay bounded (ADR-0026).
		{`(?i:sk-)[A-Za-z0-9]{20,200}`, 203, true},
		{`(?i)["']?(password|passwd|pwd)["']?\s{0,32}[:=]\s{0,32}["']?[^\s"']{4,256}`, 332, true},
		{`\b1[3-9]\d[\s-]?\d{4}[\s-]?\d{4}\b`, 13, true},
	}
	for _, c := range cases {
		got, bounded := ruleMaxMatch(c.pattern)
		if bounded != c.bounded || (bounded && got != c.want) {
			t.Errorf("ruleMaxMatch(%q) = %d,%v; want %d,%v", c.pattern, got, bounded, c.want, c.bounded)
		}
	}
}

func TestMaxMatchBytesDerivedFromRuleSet(t *testing.T) {
	long := strings.Repeat("机密", 60) // 240-byte dictionary term
	s := NewTableScanner(func(ctx context.Context) ([]Rule, error) {
		return []Rule{
			{Name: "phone", Pattern: `138[0-9]{8}`, Action: "redact"},
			{Name: "codename", Pattern: `placeholder`, Terms: []string{long}, Action: "redact"},
		}, nil
	})
	m, bounded, err := s.MaxMatchBytes(context.Background())
	if err != nil || !bounded {
		t.Fatalf("bounded rule set must derive: %v %v", m, err)
	}
	// The dictionary term (240 bytes) must dominate the phone rule (11).
	if m < len(long) {
		t.Fatalf("derived max %d must cover the %d-byte dictionary term", m, len(long))
	}
}
