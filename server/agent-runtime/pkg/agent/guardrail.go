// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// InputGuardrail validates or transforms the user input before the run.
type InputGuardrail interface {
	ValidateInput(ctx context.Context, input *session.RunInput) error
}

// OutputGuardrail validates or transforms the final run output.
type OutputGuardrail interface {
	ValidateOutput(ctx context.Context, output *session.RunOutput) error
}

// MaxLengthGuardrail rejects inputs or outputs that exceed a max length.
type MaxLengthGuardrail struct {
	// MaxInputLen rejects inputs longer than this value when greater than zero.
	MaxInputLen int
	// MaxOutputLen rejects outputs longer than this value when greater than zero.
	MaxOutputLen int
}

// ValidateInput enforces MaxInputLen on the user input.
func (g *MaxLengthGuardrail) ValidateInput(ctx context.Context, input *session.RunInput) error {
	if g.MaxInputLen > 0 && len(input.Message) > g.MaxInputLen {
		return fmt.Errorf("input exceeds max length of %d", g.MaxInputLen)
	}
	return nil
}

// ValidateOutput enforces MaxOutputLen on the run output.
func (g *MaxLengthGuardrail) ValidateOutput(ctx context.Context, output *session.RunOutput) error {
	if g.MaxOutputLen > 0 && len(output.Content) > g.MaxOutputLen {
		return fmt.Errorf("output exceeds max length of %d", g.MaxOutputLen)
	}
	return nil
}

// KeywordGuardrail rejects inputs or outputs that contain forbidden keywords.
type KeywordGuardrail struct {
	// Forbidden is the list of keywords that trigger rejection.
	Forbidden []string
}

// ValidateInput rejects inputs containing any forbidden keyword.
func (g *KeywordGuardrail) ValidateInput(ctx context.Context, input *session.RunInput) error {
	for _, kw := range g.Forbidden {
		if strings.Contains(strings.ToLower(input.Message), strings.ToLower(kw)) {
			return fmt.Errorf("input contains forbidden keyword: %q", kw)
		}
	}
	return nil
}

// ValidateOutput rejects outputs containing any forbidden keyword.
func (g *KeywordGuardrail) ValidateOutput(ctx context.Context, output *session.RunOutput) error {
	for _, kw := range g.Forbidden {
		if strings.Contains(strings.ToLower(output.Content), strings.ToLower(kw)) {
			return fmt.Errorf("output contains forbidden keyword: %q", kw)
		}
	}
	return nil
}

// Guardrails bundles multiple guardrails into a single Hooks-compatible helper.
type Guardrails struct {
	// Inputs are applied to the run input before execution.
	Inputs []InputGuardrail
	// Outputs are applied to the run output after execution.
	Outputs []OutputGuardrail
}

// AsHooks returns a Hooks struct that invokes all configured guardrails.
func (g *Guardrails) AsHooks() *Hooks {
	return &Hooks{
		PreRun:  guardrailPreRun{g.Inputs},
		PostRun: guardrailPostRun{g.Outputs},
	}
}

type guardrailPreRun struct{ guards []InputGuardrail }

func (h guardrailPreRun) PreRun(ctx context.Context, input *session.RunInput) error {
	for _, g := range h.guards {
		if err := g.ValidateInput(ctx, input); err != nil {
			return err
		}
	}
	return nil
}

type guardrailPostRun struct{ guards []OutputGuardrail }

func (h guardrailPostRun) PostRun(ctx context.Context, output *session.RunOutput) error {
	for _, g := range h.guards {
		if err := g.ValidateOutput(ctx, output); err != nil {
			return err
		}
	}
	return nil
}
