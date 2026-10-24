package requirement

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

func TestRequirement_CanResolveAt(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	tests := []struct {
		name string
		req  Requirement
		want bool
	}{
		{"pending no deadline", Requirement{Status: StatusPending}, true},
		{"pending future deadline", Requirement{Status: StatusPending, Deadline: &future}, true},
		{"pending past deadline", Requirement{Status: StatusPending, Deadline: &past}, false},
		{"approved", Requirement{Status: StatusApproved}, false},
		{"rejected", Requirement{Status: StatusRejected}, false},
		{"expired", Requirement{Status: StatusExpired}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.req.CanResolveAt(now); got != tt.want {
				t.Errorf("CanResolveAt = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequirement_IsResolved(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusPending, false},
		{StatusApproved, true},
		{StatusRejected, true},
		{StatusExpired, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			r := Requirement{Status: tt.status}
			if got := r.IsResolved(); got != tt.want {
				t.Errorf("IsResolved = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveAction(t *testing.T) {
	tests := []struct {
		raw   string
		want  Action
		valid bool
	}{
		{"approve", ActionApprove, true},
		{"reject", ActionReject, true},
		{"expire", ActionExpire, true},
		{"", "", false},
		{"invalid", "", false},
		{"APPROVE", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := ResolveAction(tt.raw)
			if ok != tt.valid || got != tt.want {
				t.Errorf("ResolveAction(%q) = (%q, %v), want (%q, %v)", tt.raw, got, ok, tt.want, tt.valid)
			}
		})
	}
}

func TestRequirement_JSONRoundTrip(t *testing.T) {
	tc := models.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: models.FunctionCall{
			Name:      "delete_file",
			Arguments: `{"path":"/etc/passwd"}`,
		},
	}
	req := Requirement{
		ID:        "req_1",
		RunID:     "run_1",
		Kind:      "approval",
		ToolCall:  tc,
		Status:    StatusPending,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Requirement
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.ID != req.ID || decoded.ToolCall.ID != tc.ID || decoded.ToolCall.Function.Name != tc.Function.Name {
		t.Errorf("round-trip mismatch: %+v", decoded)
	}
	if decoded.ToolCall.Function.Arguments != tc.Function.Arguments {
		t.Errorf("tool call arguments mismatch: %q", decoded.ToolCall.Function.Arguments)
	}
}

func TestSnapshotToolCall(t *testing.T) {
	tc := models.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: models.FunctionCall{
			Name:      "search",
			Arguments: `{"query":"test"}`,
		},
	}
	clone := SnapshotToolCall(tc)
	if clone.ID != tc.ID || clone.Function.Arguments != tc.Function.Arguments {
		t.Errorf("clone mismatch: %+v", clone)
	}
}
