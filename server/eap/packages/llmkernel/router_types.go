package llmkernel

import (
	"context"
	"encoding/json"

	"enterprise-agent-platform/packages/errs"
)

// ProviderTarget is one independently configured upstream (control data).
type ProviderTarget struct {
	Name          string
	BaseURL       string
	CredentialRef string
}

// CredentialLookup resolves a server-side credential reference at call time.
// Failures are observable errors — never an unauthenticated request.
type CredentialLookup func(ctx context.Context, ref string) (string, error)

// RouteAttempt describes one tried route for trace/audit evidence.
type RouteAttempt struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Status   string `json:"status"` // selected | failed
	Err      string `json:"error,omitempty"`
}

// scanWithMode uses the richer ModeScanner contract when available so
// allow_with_redaction can downgrade deny-class matches; otherwise it falls
// back to the plain scan (enforce semantics — safe for simple scanners).
func scanWithMode(ctx context.Context, dlp DLPEngine, text string, mode ScanMode) (DLPResult, error) {
	if mode == ScanRedact {
		if ms, ok := dlp.(ModeScanner); ok {
			return ms.ScanWithMode(ctx, text, mode)
		}
	}
	return dlp.Scan(ctx, text)
}

// applyRedactions returns the redacted message content. The scanner always
// returns the full redacted text for the input it scanned, so mapping is
// exact per message. If the scanner produced no redacted text (nothing to
// redact) the original content passes through unchanged.
func applyRedactions(content string, scan DLPResult) string {
	if scan.Redacted == "" {
		return content
	}
	return scan.Redacted
}

// scanMessages scans every message individually and applies redactions in
// place, returning the total redaction count and matched rule names. Exact
// per-message redaction avoids offset-mapping bugs from concatenation.
func scanMessages(ctx context.Context, dlp DLPEngine, req ChatRequest, mode ScanMode) (DLPResult, error) {
	total := DLPResult{}
	var firstErr error
	for i := range req.Messages {
		res, err := scanWithMode(ctx, dlp, req.Messages[i].Content, mode)
		if err != nil {
			return total, err
		}
		blocked := res.Blocked
		if firstErr == nil && blocked {
			total.Blocked = true
			total.Reason = res.Reason
			total.RuleNames = append(total.RuleNames, res.RuleNames...)
			total.RedactionCount += res.RedactionCount
		} else {
			if res.RedactionCount > 0 {
				req.Messages[i].Content = applyRedactions(req.Messages[i].Content, res)
				rewritePartsText(ctx, dlp, &req.Messages[i], mode)
			}
			total.RedactionCount += res.RedactionCount
			total.RuleNames = append(total.RuleNames, res.RuleNames...)
			if blocked && firstErr == nil {
				total.Blocked = true
				total.Reason = res.Reason
			}
		}
		// Tool-call arguments are provider-bound conversation data: they are
		// scanned like content, and redactions are written back into the
		// arguments string (ADR-0025).
		for j := range req.Messages[i].ToolCalls {
			res, err := scanWithMode(ctx, dlp, req.Messages[i].ToolCalls[j].Arguments, mode)
			if err != nil {
				return total, err
			}
			if firstErr == nil && res.Blocked {
				total.Blocked = true
				total.Reason = res.Reason
				total.RuleNames = append(total.RuleNames, res.RuleNames...)
				total.RedactionCount += res.RedactionCount
				continue
			}
			if res.RedactionCount > 0 {
				req.Messages[i].ToolCalls[j].Arguments = applyRedactions(req.Messages[i].ToolCalls[j].Arguments, res)
			}
			total.RedactionCount += res.RedactionCount
			total.RuleNames = append(total.RuleNames, res.RuleNames...)
			if res.Blocked && firstErr == nil {
				total.Blocked = true
				total.Reason = res.Reason
			}
		}
	}
	return total, nil
}

// rewritePartsText rewrites the text parts of a structurally preserved
// content-parts message after DLP redacted the flattened content
// (ADR-0028 decision 2: text governance stays on the flattened scan; the
// forwarded structure must not reintroduce what the scan removed). Each
// text part is re-scanned to obtain its redacted form — per-part counts and
// blocking are ignored because governance was already decided on the
// flattened scan. Non-text parts pass through untouched (documented V1
// stance).
func rewritePartsText(ctx context.Context, dlp DLPEngine, msg *ChatMessage, mode ScanMode) {
	for i := range msg.ContentParts {
		p := &msg.ContentParts[i]
		if p.Type != "text" && p.Type != "" {
			continue
		}
		res, err := scanWithMode(ctx, dlp, p.Text, mode)
		if err != nil || res.Redacted == "" || res.Redacted == p.Text {
			continue
		}
		p.Text = res.Redacted
		p.Raw = rewriteRawTextField(p.Raw, res.Redacted)
	}
}

// rewriteRawTextField returns part JSON with the "text" field replaced,
// preserving every other field byte-for-byte.
func rewriteRawTextField(raw json.RawMessage, text string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	b, err := json.Marshal(text)
	if err != nil {
		return raw
	}
	m["text"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

var _ = errs.New

// StreamClient is the optional streaming contract on a ProviderClient.
type StreamClient interface {
	ChatStream(ctx context.Context, target ProviderTarget, credential string, req ChatRequest) (<-chan StreamChunk, <-chan error)
}
