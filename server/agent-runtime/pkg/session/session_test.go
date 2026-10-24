package session

import "testing"

func TestRunStatusTransitions(t *testing.T) {
	tests := []struct {
		name string
		from RunStatus
		to   RunStatus
		want bool
	}{
		{"pending starts", RunStatusPending, RunStatusRunning, true},
		{"running completes", RunStatusRunning, RunStatusCompleted, true},
		{"running fails", RunStatusRunning, RunStatusError, true},
		{"running cancels", RunStatusRunning, RunStatusCancelled, true},
		{"running pauses", RunStatusRunning, RunStatusPaused, true},
		{"paused resumes", RunStatusPaused, RunStatusRunning, true},
		{"paused cancels", RunStatusPaused, RunStatusCancelled, true},
		{"terminal cannot restart", RunStatusCompleted, RunStatusRunning, false},
		{"terminal is idempotent", RunStatusCancelled, RunStatusCancelled, true},
		{"running is idempotent", RunStatusRunning, RunStatusRunning, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.from.CanTransitionTo(tt.to); got != tt.want {
				t.Fatalf("%q -> %q = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}
