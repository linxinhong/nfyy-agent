package session

import (
	"encoding/json"
	"testing"
)

func TestRunOutputPendingPlansRoundTrip(t *testing.T) {
	out := RunOutput{
		RunID: "r1",
		PendingPlans: map[string]PendingPlanRef{
			"call-1": {ID: "plan-1", Version: "v1", ApprovalRequired: true},
			"call-2": {ID: "plan-2", Version: "v1"},
		},
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back RunOutput
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.PendingPlans) != 2 {
		t.Fatalf("pending plans = %d, want 2", len(back.PendingPlans))
	}
	if back.PendingPlans["call-1"].ID != "plan-1" || back.PendingPlans["call-1"].Version != "v1" || !back.PendingPlans["call-1"].ApprovalRequired {
		t.Fatalf("round-trip call-1 = %+v", back.PendingPlans["call-1"])
	}
	if back.PendingPlans["call-2"].ID != "plan-2" {
		t.Fatalf("round-trip call-2 = %+v", back.PendingPlans["call-2"])
	}
}
