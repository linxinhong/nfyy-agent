package errs_test

import (
	"errors"
	"net/http"
	"testing"

	"enterprise-agent-platform/internal/golden"
	"enterprise-agent-platform/packages/errs"
)

func TestProblemGoldenFixture(t *testing.T) {
	p := errs.Problem{
		Code:      errs.WorkspaceForbidden,
		Message:   "principal is not a member of workspace finance",
		TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736",
		Retriable: false,
	}
	golden.Check(t, "../../contracts/schemas/v1/error/problem.example.json", p)
}

func TestHTTPStatusMapping(t *testing.T) {
	cases := map[errs.Code]int{
		errs.InvalidArgument:     http.StatusBadRequest,
		errs.Unauthenticated:     http.StatusUnauthorized,
		errs.PermissionDenied:    http.StatusForbidden,
		errs.WorkspaceForbidden:  http.StatusForbidden,
		errs.PolicyDenied:        http.StatusForbidden,
		errs.NotFound:            http.StatusNotFound,
		errs.Conflict:            http.StatusConflict,
		errs.DLPBlocked:          http.StatusUnprocessableEntity,
		errs.RateLimited:         http.StatusTooManyRequests,
		errs.QuotaExceeded:       http.StatusTooManyRequests,
		errs.UpstreamUnavailable: http.StatusBadGateway,
		errs.Internal:            http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := code.HTTPStatus(); got != want {
			t.Errorf("code %s: got status %d, want %d", code, got, want)
		}
	}
	if got := errs.Code("brand_new_code").HTTPStatus(); got != http.StatusInternalServerError {
		t.Errorf("unknown codes must fail closed to 500, got %d", got)
	}
}

func TestFailClosedConstructors(t *testing.T) {
	cases := []struct {
		name string
		err  *errs.Error
		want errs.Code
	}{
		{"policy", errs.PolicyFailClosed("engine timeout"), errs.PolicyDenied},
		{"dlp", errs.DLPFailClosed("scanner unavailable"), errs.DLPBlocked},
		{"workspace", errs.WorkspaceFailClosed("membership store down"), errs.WorkspaceForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := errs.From(tc.err)
			if !ok {
				t.Fatalf("expected an *errs.Error")
			}
			if p.Code != tc.want {
				t.Fatalf("got code %s, want %s", p.Code, tc.want)
			}
			if p.Retriable {
				t.Fatalf("fail-closed denials must not be marked retriable")
			}
		})
	}
}

func TestWrapPreservesCause(t *testing.T) {
	cause := errors.New("connection refused")
	err := errs.Wrap(cause, errs.UpstreamUnavailable, "provider call failed")
	if !errors.Is(err, cause) {
		t.Fatalf("Wrap must preserve the cause chain")
	}
	p, ok := errs.From(err)
	if !ok || p.Code != errs.UpstreamUnavailable {
		t.Fatalf("From must recover the problem: %+v", p)
	}
	if got := err.Error(); got == "" {
		t.Fatalf("Error() must render a message")
	}
}

func TestFromUnknownError(t *testing.T) {
	if _, ok := errs.From(errors.New("plain")); ok {
		t.Fatalf("plain errors carry no Problem")
	}
}
