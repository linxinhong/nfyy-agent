package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// calibratedCounter scales an estimating TokenCounter by the observed ratio
// of provider-reported prompt tokens to the estimate. The provider count
// includes what estimates miss (tool schemas, chat-template overhead) and
// corrects tokenizer skew (e.g. Chinese text), so budgets track reality.
type calibratedCounter struct {
	base TokenCounter
	// overhead estimates what every request carries besides messages (the
	// tool schemas), so the ratio corrects tokenizer skew rather than
	// absorbing a fixed cost that would inflate long contexts.
	overhead int

	mu    sync.Mutex
	ratio float64 // 0 until the first observation
}

const (
	calibrationWeight = 0.2
	minCalibration    = 0.5
	maxCalibration    = 3.0
)

func newCalibratedCounter(toolSchemas []models.Tool) *calibratedCounter {
	c := &calibratedCounter{base: ApproximateTokenCounter{}}
	if len(toolSchemas) > 0 {
		if raw, err := json.Marshal(toolSchemas); err == nil {
			c.overhead = approximateTextTokens(string(raw))
		}
	}
	return c
}

// Count implements TokenCounter.
func (c *calibratedCounter) Count(ctx context.Context, messages []models.Message) (int, error) {
	n, err := c.base.Count(ctx, messages)
	if err != nil {
		return n, err
	}
	n += c.overhead
	if r := c.currentRatio(); r > 0 {
		n = int(float64(n)*r + 0.5)
	}
	return n, nil
}

func (c *calibratedCounter) currentRatio() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ratio
}

// observe folds one (estimate, actual) pair into the ratio.
func (c *calibratedCounter) observe(estimated, actual int) {
	if estimated <= 0 || actual <= 0 {
		return
	}
	r := min(max(float64(actual)/float64(estimated), minCalibration), maxCalibration)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ratio == 0 {
		c.ratio = r
	} else {
		c.ratio = (1-calibrationWeight)*c.ratio + calibrationWeight*r
	}
}

// calibrate records the prompt size the provider reported for messages.
// Only the primary model's reports count: a fallback may tokenize
// differently, and the budget is mostly spent on the primary.
func (a *Agent) calibrate(ctx context.Context, served models.Model, messages []models.Message, usage *models.MessageMetrics) {
	if a.calibration == nil || usage == nil || usage.InputTokens <= 0 {
		return
	}
	if served != nil && served != a.cfg.Model {
		return
	}
	estimated, err := a.calibration.base.Count(ctx, messages)
	if err == nil {
		a.calibration.observe(estimated+a.calibration.overhead, usage.InputTokens)
	}
}

// NewModelSummarizer returns a Summarizer that asks model for the summary.
// The conversation is sent as a plain transcript in one user message, so
// tool-call pairing and reasoning-replay rules of the provider never apply.
// A small, cheap model is usually enough.
func NewModelSummarizer(model models.Model) Summarizer {
	return &modelSummarizer{model: model}
}

type modelSummarizer struct{ model models.Model }

const summarizerPrompt = `Summarize the conversation transcript for an assistant that will continue it. Preserve facts, decisions, user preferences, open questions, and the outcomes of tool calls. Be concise and factual; do not add anything that is not in the transcript.`

func (s *modelSummarizer) Summarize(ctx context.Context, messages []models.Message) (string, error) {
	var b strings.Builder
	for _, m := range messages {
		switch {
		case m.Role == models.RoleTool:
			fmt.Fprintf(&b, "[tool result %s]: %s\n", m.Name, m.Content)
		case len(m.ToolCalls) > 0:
			if m.Content != "" {
				fmt.Fprintf(&b, "[%s]: %s\n", m.Role, m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[%s called %s]: %s\n", m.Role, tc.Function.Name, tc.Function.Arguments)
			}
		default:
			fmt.Fprintf(&b, "[%s]: %s\n", m.Role, m.Content)
		}
	}
	resp, err := s.model.Response(ctx, []models.Message{
		{Role: models.RoleSystem, Content: summarizerPrompt},
		{Role: models.RoleUser, Content: b.String()},
	}, models.ModelOptions{})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.Content) == "" {
		return "", fmt.Errorf("agent: summarizer returned no content")
	}
	return resp.Content, nil
}
