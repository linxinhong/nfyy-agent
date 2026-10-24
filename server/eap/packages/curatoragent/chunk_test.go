package curatoragent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitMarkdownKeepsSectionsWholeAndLossless(t *testing.T) {
	intro := "判决摘要。\n\n"
	facts := "## 一、案件事实\n" + strings.Repeat("原告与被告签订对赌协议。", 30) + "\n\n"
	code := "```\n# 不是标题\n```\n"
	reasoning := "## 二、裁判理由\n" + strings.Repeat("回购条款不违反法律强制性规定。", 30) + "\n" + code
	long := "## 三、附件\n" + strings.Repeat("附件段落。", 50) + "\n\n" + strings.Repeat("超长段落没有空行", 200)
	text := intro + facts + reasoning + long

	sections := splitMarkdown(text, 600)
	var joined strings.Builder
	for i, s := range sections {
		joined.WriteString(s.Text)
		if n := utf8.RuneCountInString(s.Text); n > 600 {
			t.Fatalf("section %d has %d runes", i, n)
		}
		if !utf8.ValidString(s.Text) {
			t.Fatalf("section %d cut inside a rune", i)
		}
	}
	if joined.String() != text {
		t.Fatal("sections must reproduce the input exactly")
	}
	if sections[0].Text != intro+facts || sections[0].Heading != "一、案件事实" {
		t.Fatalf("small units pack until a section is full: %q", sections[0].Heading)
	}
	if sections[1].Heading != "二、裁判理由" || !strings.HasSuffix(sections[1].Text, code) {
		t.Fatalf("a heading inside a code fence does not split: %+v", sections[1])
	}
	if len(sections) < 4 || sections[2].Heading != "三、附件" {
		t.Fatalf("an oversized section splits by paragraph then hard cut: %d sections", len(sections))
	}
}

func TestOutlineNamesEverySection(t *testing.T) {
	got := outline([]section{{Heading: "事实", Text: "## 事实\n原告主张回购。"}, {Text: "无标题段落"}})
	if !strings.Contains(got, "[0] 事实") || !strings.Contains(got, "原告主张回购") || !strings.Contains(got, "[1] （无标题）") {
		t.Fatalf("outline: %s", got)
	}
}
