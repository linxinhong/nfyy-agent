package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

func TestClassifyMapsSentinels(t *testing.T) {
	cases := []struct {
		err    error
		code   Code
		status int
	}{
		{service.ErrForbidden, CodeForbidden, http.StatusForbidden},
		{service.ErrUnknownTool, CodeUnknownTool, http.StatusNotFound},
		{service.ErrPlanNotFound, CodeNotFound, http.StatusNotFound},
		{service.ErrPlanStale, CodeStale, http.StatusConflict},
		{service.ErrGrantExpired, CodeExpired, http.StatusGone},
		{service.ErrApprovalRequired, CodeApprovalRequired, http.StatusAccepted},
		{service.ErrResultTooLarge, CodeResultTooLarge, http.StatusUnprocessableEntity},
		{db.ErrConflict, CodeConflict, http.StatusConflict},
		{run.ErrLeaseHeld, CodeLeaseHeld, http.StatusConflict},
		{run.ErrStaleFencingToken, CodeStale, http.StatusConflict},
		{errors.New("unknown"), CodeInternal, http.StatusInternalServerError},
	}
	for _, c := range cases {
		code, status := Classify(c.err)
		if code != c.code || status != c.status {
			t.Errorf("Classify(%v) = (%s, %d), want (%s, %d)", c.err, code, status, c.code, c.status)
		}
	}
}

func TestClassifyWrappedErrors(t *testing.T) {
	code, _ := Classify(errors.Join(service.ErrPlanExpired, errors.New("detail")))
	if code != CodeExpired {
		t.Fatalf("wrapped classify = %s, want expired", code)
	}
}
