// Heading-aware splitting of long Markdown for the curator's tools.
//
// Why: a contribution may carry up to 200 KB of Markdown (judgments,
// contracts, statutes), far beyond one prompt. Long texts are shown to the
// model as an outline and read section by section, so a section should be a
// whole Markdown section where possible: split at headings first, then at
// blank-line paragraphs, and hard-cut only a paragraph that alone is too long.
//
// Invariants: concatenating the sections reproduces the input exactly (no
// overlap, no trimming), and every cut falls on a rune boundary.
package curatoragent

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// inlineRunes is the longest text shown to the model in full.
	inlineRunes = 12_000
	// sectionRunes is the target size of one readable section.
	sectionRunes = 6_000
)

type section struct {
	Heading string
	Text    string
}

// splitMarkdown cuts text into sections of at most target runes.
func splitMarkdown(text string, target int) []section {
	var out []section
	for _, unit := range headingUnits(text) {
		if utf8.RuneCountInString(unit) <= target {
			out = appendUnit(out, unit, target)
			continue
		}
		for _, para := range paragraphUnits(unit) {
			for _, piece := range hardCut(para, target) {
				out = appendUnit(out, piece, target)
			}
		}
	}
	for i := range out {
		out[i].Heading = headingOf(out[i].Text)
	}
	return out
}

// appendUnit packs small consecutive units into one section.
func appendUnit(out []section, unit string, target int) []section {
	if n := len(out); n > 0 && utf8.RuneCountInString(out[n-1].Text)+utf8.RuneCountInString(unit) <= target {
		out[n-1].Text += unit
		return out
	}
	return append(out, section{Text: unit})
}

// headingUnits splits before every ATX heading line outside a code fence.
func headingUnits(text string) []string {
	var units []string
	start, fenced := 0, false
	for pos := 0; pos < len(text); {
		end := strings.IndexByte(text[pos:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += pos + 1
		}
		line := strings.TrimLeft(text[pos:end], " ")
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fenced = !fenced
		} else if !fenced && isHeading(line) && pos > start {
			units = append(units, text[start:pos])
			start = pos
		}
		pos = end
	}
	if start < len(text) {
		units = append(units, text[start:])
	}
	return units
}

func isHeading(line string) bool {
	hashes := len(line) - len(strings.TrimLeft(line, "#"))
	return hashes >= 1 && hashes <= 6 && len(line) > hashes && (line[hashes] == ' ' || line[hashes] == '\t')
}

// paragraphUnits splits after every blank line, keeping the separators.
func paragraphUnits(text string) []string {
	var units []string
	for {
		i := strings.Index(text, "\n\n")
		if i < 0 {
			break
		}
		units = append(units, text[:i+2])
		text = text[i+2:]
	}
	if text != "" {
		units = append(units, text)
	}
	return units
}

func hardCut(text string, target int) []string {
	var pieces []string
	for utf8.RuneCountInString(text) > target {
		cut, n := 0, 0
		for i := range text {
			if n == target {
				cut = i
				break
			}
			n++
		}
		pieces = append(pieces, text[:cut])
		text = text[cut:]
	}
	return append(pieces, text)
}

func headingOf(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if isHeading(line) {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
	}
	return ""
}

// outline lists the sections for the prompt: index, heading, size and opening.
func outline(sections []section) string {
	var b strings.Builder
	for i, s := range sections {
		heading := s.Heading
		if heading == "" {
			heading = "（无标题）"
		}
		fmt.Fprintf(&b, "  [%d] %s（%d 字）%s\n", i, heading, utf8.RuneCountInString(s.Text), opening(s.Text, 60))
	}
	return b.String()
}

// opening is the first non-heading line, cut to max runes.
func opening(text string, max int) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || isHeading(line) {
			continue
		}
		if utf8.RuneCountInString(line) > max {
			line = string([]rune(line)[:max]) + "…"
		}
		return "：" + line
	}
	return ""
}
