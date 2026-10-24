package providers

import (
	"bytes"
	"strings"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// ReasoningReplay selects which assistant reasoning_content is sent back to
// an OpenAI-compatible endpoint.
type ReasoningReplay string

const (
	// ReasoningReplayAuto picks a per-model default (see OpenAICompletionsCompat).
	ReasoningReplayAuto ReasoningReplay = ""
	// ReasoningReplayAll sends reasoning_content on every assistant message.
	// The official DeepSeek API requires it for assistant tool-call messages
	// in thinking mode and ignores it elsewhere.
	ReasoningReplayAll ReasoningReplay = "all"
	// ReasoningReplayCurrentTurn sends reasoning only for assistant messages
	// after the last user message (the in-flight tool loop).
	ReasoningReplayCurrentTurn ReasoningReplay = "current_turn"
	// ReasoningReplayNone never sends reasoning_content. Original DeepSeek-R1
	// deployments reject requests whose messages carry it.
	ReasoningReplayNone ReasoningReplay = "none"
)

// modelFamily is the detected behavior family of a model, independent of
// which provider hosts it.
type modelFamily int

const (
	familyGeneric modelFamily = iota
	// familyDeepSeekV3 covers deepseek-chat and DeepSeek-V3/V3.1/V3.2 weights.
	familyDeepSeekV3
	// familyDeepSeekR1 covers deepseek-reasoner, DeepSeek-R1(-0528) and the
	// R1 distills, which may emit <think> blocks inside content.
	familyDeepSeekR1
)

// detectFamily classifies a model by ID across hosts, e.g.
// "deepseek-reasoner", "deepseek-ai/DeepSeek-R1", "Pro/deepseek-ai/DeepSeek-V3",
// "deepseek-r1-250528" (Volcengine Ark), "deepseek-r1:14b" (Ollama),
// "DeepSeek-R1-Distill-Qwen-32B".
func detectFamily(m Model) modelFamily {
	id := strings.ToLower(m.ID)
	if !strings.Contains(id, "deepseek") && m.Provider != "deepseek" {
		return familyGeneric
	}
	if strings.Contains(id, "reasoner") || strings.Contains(id, "r1") {
		return familyDeepSeekR1
	}
	return familyDeepSeekV3
}

// isOfficialDeepSeek reports whether the model is served through the
// registered "deepseek" provider or api.deepseek.com.
func isOfficialDeepSeek(m Model) bool {
	return m.Provider == "deepseek" || strings.Contains(strings.ToLower(m.BaseURL), "api.deepseek.com")
}

// reasoningReplay resolves the effective replay policy.
func (c *openAICompletionsChat) reasoningReplay() ReasoningReplay {
	if c.model.Compat != nil && c.model.Compat.ReasoningReplay != ReasoningReplayAuto {
		return c.model.Compat.ReasoningReplay
	}
	if detectFamily(c.model) == familyDeepSeekR1 && !isOfficialDeepSeek(c.model) {
		return ReasoningReplayNone
	}
	return ReasoningReplayAll
}

// parseThinkTags resolves whether <think> blocks in content are split out
// into reasoning. Auto: on for the DeepSeek-R1 family on any host.
func (c *openAICompletionsChat) parseThinkTags() bool {
	if c.model.Compat != nil && c.model.Compat.ParseThinkTags != nil {
		return *c.model.Compat.ParseThinkTags
	}
	return detectFamily(c.model) == familyDeepSeekR1
}

// applyReasoningReplay strips reasoning from messages per policy.
func applyReasoningReplay(msgs []chatCompletionMessage, policy ReasoningReplay) {
	switch policy {
	case ReasoningReplayNone:
		for i := range msgs {
			msgs[i].ReasoningContent = ""
		}
	case ReasoningReplayCurrentTurn:
		lastUser := -1
		for i := range msgs {
			if msgs[i].Role == string(models.RoleUser) {
				lastUser = i
			}
		}
		for i := 0; i < lastUser; i++ {
			msgs[i].ReasoningContent = ""
		}
	}
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// splitThinkTags separates a leading <think>…</think> block from content.
// It also handles R1 deployments whose chat template pre-fills the opening
// tag, so the output starts directly with reasoning and only "</think>"
// appears. Content without think markers is returned unchanged.
func splitThinkTags(content string) (reasoning, answer string) {
	trimmed := strings.TrimLeft(content, " \t\r\n")
	if strings.HasPrefix(trimmed, thinkOpen) {
		rest := trimmed[len(thinkOpen):]
		if end := strings.Index(rest, thinkClose); end >= 0 {
			return strings.TrimSpace(rest[:end]), strings.TrimLeft(rest[end+len(thinkClose):], " \t\r\n")
		}
		return strings.TrimSpace(rest), ""
	}
	if end := strings.Index(content, thinkClose); end >= 0 && !strings.Contains(content[:end], thinkOpen) {
		return strings.TrimSpace(content[:end]), strings.TrimLeft(content[end+len(thinkClose):], " \t\r\n")
	}
	return "", content
}

type thinkState int

const (
	thinkStart thinkState = iota
	thinkInside
	thinkAfter
	// thinkMaybePrefilled holds content that did not open with <think>:
	// with a pre-filled template it is reasoning up to "</think>".
	thinkMaybePrefilled
)

// thinkStreamParser incrementally routes streamed content inside a leading
// <think> block to reasoning. Tags split across chunks are handled by
// holding back the shortest ambiguous suffix. Streams whose template
// pre-fills the opening tag pass through as content (no buffering), since
// waiting for "</think>" would stall every non-thinking answer.
type thinkStreamParser struct {
	state   thinkState
	pending []byte
	// scanned is how much of pending was already searched for "</think>"
	// while holding possibly pre-filled reasoning, so each delta costs its
	// own length rather than the whole held text.
	scanned int
	// trimAnswer drops whitespace between "</think>" and the answer.
	trimAnswer bool
}

// maxPrefilledHold bounds how much untagged content is held back waiting
// for a "</think>" that would mark it as pre-filled reasoning.
const maxPrefilledHold = 32 << 10

// newThinkStreamParser returns a parser. prefilled starts inside the think
// block, for deployments known to pre-fill the opening tag, so reasoning
// streams live instead of being held until "</think>".
func newThinkStreamParser(prefilled bool) *thinkStreamParser {
	if prefilled {
		return &thinkStreamParser{state: thinkInside}
	}
	return &thinkStreamParser{}
}

// sawReasoningField records that the endpoint streams reasoning in a
// separate field, so content cannot be pre-filled reasoning.
func (p *thinkStreamParser) sawReasoningField() {
	if p.state == thinkStart || p.state == thinkMaybePrefilled {
		p.state = thinkAfter
	}
}

// feed consumes one content delta and returns the reasoning and content
// portions that are now unambiguous.
func (p *thinkStreamParser) feed(delta string) (reasoning, content string) {
	p.pending = append(p.pending, delta...)
	var rb, cb strings.Builder
	for {
		switch p.state {
		case thinkStart:
			trimmed := bytes.TrimLeft(p.pending, " \t\r\n")
			if len(trimmed) == 0 || (len(trimmed) < len(thinkOpen) && strings.HasPrefix(thinkOpen, string(trimmed))) {
				return rb.String(), cb.String()
			}
			if bytes.HasPrefix(trimmed, []byte(thinkOpen)) {
				p.pending = trimmed[len(thinkOpen):]
				p.state = thinkInside
				continue
			}
			p.state = thinkMaybePrefilled
			continue
		case thinkMaybePrefilled:
			from := max(p.scanned-len(thinkClose)+1, 0)
			if end := bytes.Index(p.pending[from:], []byte(thinkClose)); end >= 0 {
				end += from
				rb.Write(bytes.TrimSpace(p.pending[:end]))
				p.pending = p.pending[end+len(thinkClose):]
				p.state = thinkAfter
				p.trimAnswer = true
				continue
			}
			if len(p.pending) > maxPrefilledHold {
				p.state = thinkAfter
				continue
			}
			p.scanned = len(p.pending)
			return rb.String(), cb.String()
		case thinkInside:
			if end := bytes.Index(p.pending, []byte(thinkClose)); end >= 0 {
				rb.Write(p.pending[:end])
				p.pending = p.pending[end+len(thinkClose):]
				p.state = thinkAfter
				p.trimAnswer = true
				continue
			}
			keep := partialSuffix(p.pending, thinkClose)
			rb.Write(p.pending[:len(p.pending)-keep])
			p.pending = append(p.pending[:0], p.pending[len(p.pending)-keep:]...)
			return rb.String(), cb.String()
		default:
			if p.trimAnswer {
				p.pending = bytes.TrimLeft(p.pending, " \t\r\n")
				if len(p.pending) == 0 {
					return rb.String(), cb.String()
				}
				p.trimAnswer = false
			}
			cb.Write(p.pending)
			p.pending = p.pending[:0]
			return rb.String(), cb.String()
		}
	}
}

// flush returns whatever is still held back at end of stream.
func (p *thinkStreamParser) flush() (reasoning, content string) {
	rest := string(p.pending)
	p.pending = nil
	switch p.state {
	case thinkInside:
		return rest, ""
	case thinkMaybePrefilled:
		// No "</think>" arrived: it was an ordinary answer.
		return "", rest
	case thinkAfter:
		if p.trimAnswer {
			return "", strings.TrimLeft(rest, " \t\r\n")
		}
	}
	return "", rest
}

// partialSuffix returns the length of the longest suffix of s that is a
// proper prefix of tag.
func partialSuffix(s []byte, tag string) int {
	max := len(tag) - 1
	if max > len(s) {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if bytes.HasSuffix(s, []byte(tag[:n])) {
			return n
		}
	}
	return 0
}
