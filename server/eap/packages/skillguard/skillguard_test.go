package skillguard

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const skill = `# 对赌回购审查
1. 先看回购触发条件是否约定了明确的业绩指标和考核期间，避免触发条件模糊导致争议。
2. 核对回购义务主体：目标公司回购需履行减资程序，股东回购不受此限。`

func TestRedactMasksCopiedRunsEvenWhenReformatted(t *testing.T) {
	g := New(skill)
	out, n := g.Redact("我的结论是可以回购。要点：先看回购触发条件 是否约定了**明确的业绩指标**和考核期间，避免触发条件模糊导致争议。其余正常。")
	if n != 1 || strings.Contains(out, "业绩指标") || !strings.Contains(out, Mask) || !strings.Contains(out, "我的结论是可以回购") || !strings.HasSuffix(out, "其余正常。") {
		t.Fatalf("masked %d: %s", n, out)
	}
}

func TestShortQuotesAndUnrelatedTextPass(t *testing.T) {
	g := New(skill)
	for _, text := range []string{
		"需要核对回购义务主体。",
		"目标公司回购需要履行减资程序吗？一般来说需要。",
		"本案业绩承诺未达标，投资人主张回购。",
	} {
		if out, n := g.Redact(text); n != 0 || out != text {
			t.Fatalf("%q masked: %s", text, out)
		}
	}
	if g.Leaks(`{"path":"notes.md","content":"回购"}`) {
		t.Fatal("short tool arguments do not leak")
	}
	if !g.Leaks(`{"content":"核对回购义务主体：目标公司回购需履行减资程序，股东回购不受此限"}`) {
		t.Fatal("copied tool arguments leak")
	}
}

func TestNothingToGuard(t *testing.T) {
	if New("太短") != nil || New() != nil {
		t.Fatal("texts shorter than the window guard nothing")
	}
	var g *Guard
	if out, n := g.Redact("任意文本"); n != 0 || out != "任意文本" || g.Leaks("x") {
		t.Fatal("a nil guard passes everything")
	}
}

func TestHoldbackCoversAWindow(t *testing.T) {
	run := strings.Repeat("回，", Window)
	if len(run) > HoldbackBytes {
		t.Fatalf("a %d-letter run with punctuation spans %d bytes > holdback", Window, len(run))
	}
}

func TestContinuationAfterAMaskIsMaskedAndMasksMerge(t *testing.T) {
	g := New(skill)
	// What a stream window sees after the start of a copy was masked earlier.
	out, n := g.Redact(Mask + "导致争议。\n2. " + Mask + "股东回购不受此限。其余是我的分析。")
	if out != Mask+"。其余是我的分析。" || n != 2 {
		t.Fatalf("continuations and merge: %q (%d)", out, n)
	}
	if out, _ := g.Redact(Mask + "，本案另有约定。"); out != Mask+"，本案另有约定。" {
		t.Fatalf("text that does not continue the skill stays: %q", out)
	}
}

func TestStreamedCopyLeavesNoTail(t *testing.T) {
	g := New(skill)
	// Emulate the gateway's window: scan the held tail plus each delta and
	// release all but the last HoldbackBytes.
	var released, window string
	runes := []rune(skill)
	for i := 0; i < len(runes); i += 7 {
		window += string(runes[i:min(i+7, len(runes))])
		window, _ = g.Redact(window)
		if cut := len(window) - HoldbackBytes; cut > 0 {
			for cut > 0 && !utf8.RuneStart(window[cut]) {
				cut--
			}
			released, window = released+window[:cut], window[cut:]
		}
	}
	tail, _ := g.Redact(window)
	out := released + tail
	norm, _ := normalize(strings.ReplaceAll(out, Mask, ""))
	if len(norm) > 4 {
		t.Fatalf("streamed copy left text behind: %q", out)
	}
}
