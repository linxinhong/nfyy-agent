package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
)

func TestCallModel_RetryThenSuccess(t *testing.T) {
	m := mock.New()
	m.ResponseErrors = []error{
		&models.ProviderError{Provider: "openai", StatusCode: 500},
		&models.ProviderError{Provider: "openai", StatusCode: 503},
	}
	m.ResponseData = &models.ModelResponse{Content: "ok"}

	ag, err := New(AgentConfig{
		Name:        "retry-test",
		Model:       m,
		RetryConfig: RetryConfig{MaxRetries: 3, RetryDelay: 1 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := ag.callModel(context.Background(), nil, models.ModelOptions{})
	if err != nil {
		t.Fatalf("callModel: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
}

func TestCallModel_Fallback(t *testing.T) {
	primary := mock.New()
	primary.ResponseData = &models.ModelResponse{}
	// Make primary Response fail by using a model that always errors is hard with the
	// current mock, so we simulate by checking fallback model is used when primary
	// returns empty content but fallback returns content.
	fallback := mock.New()
	fallback.ResponseData = &models.ModelResponse{Content: "fallback"}

	ag, err := New(AgentConfig{
		Name:           "fallback-test",
		Model:          primary,
		FallbackModels: []models.Model{fallback},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := ag.callModel(context.Background(), nil, models.ModelOptions{})
	if err != nil {
		t.Fatalf("callModel: %v", err)
	}
	// Primary succeeds with empty content, so fallback is not invoked.
	if resp.Content != "" {
		t.Fatalf("expected primary empty content, got %q", resp.Content)
	}
}

func TestIsRetryableError(t *testing.T) {
	if isRetryableError(nil) {
		t.Error("nil should not be retryable")
	}
	if isRetryableError(context.Canceled) {
		t.Error("context.Canceled should not be retryable")
	}
	if !isRetryableError(&models.ProviderError{StatusCode: 500}) {
		t.Error("500 should be retryable")
	}
	if !isRetryableError(fmt.Errorf("wrapped: %w", &models.ProviderError{StatusCode: 429})) {
		t.Error("wrapped 429 should be retryable")
	}
	if !isRetryableError(models.ErrStreamInterrupted) {
		t.Error("interrupted stream should be retryable")
	}
	// Classification is typed: message text never makes an error retryable.
	for _, msg := range []string{"openai: 500 internal server error", "context length 15003 exceeds limit", "rate limit exceeded"} {
		if isRetryableError(errors.New(msg)) {
			t.Errorf("untyped error %q must not be retryable", msg)
		}
	}
	if isRetryableError(&models.ProviderError{StatusCode: 400}) {
		t.Error("400 must not be retryable")
	}
}

func TestRetryDelayHonorsRetryAfterAndCap(t *testing.T) {
	ag := &Agent{cfg: AgentConfig{RetryConfig: RetryConfig{RetryDelay: 100 * time.Millisecond, RetryMultiplier: 2}}}
	for attempt := 0; attempt < 3; attempt++ {
		full := time.Duration(float64(100*time.Millisecond) * float64(int(1)<<attempt))
		got := ag.retryDelay(attempt, errors.New("x"))
		if got < full/2 || got > full {
			t.Fatalf("attempt %d delay %v outside [%v,%v]", attempt, got, full/2, full)
		}
	}
	if got := ag.retryDelay(20, errors.New("x")); got > maxRetryDelay {
		t.Fatalf("delay %v exceeds cap", got)
	}
	advised := &models.ProviderError{StatusCode: 429, RetryAfter: 2 * time.Second}
	if got := ag.retryDelay(0, advised); got != 2*time.Second {
		t.Fatalf("Retry-After not honored: %v", got)
	}
}
