package wiki

import (
	"net/url"
	"strings"
	"testing"
)

func TestRenderSanitizesAndRewritesWikiLinks(t *testing.T) {
	src := "## 要点\n见 [[诉讼/股权纠纷/裁判观点/回购条款|回购条款]] 与 [[诉讼/股权纠纷/裁判观点/回购条款]]。\n\n" +
		"<script>alert(1)</script><img src=x onerror=alert(1)>\n\n[外链](https://example.com) [危险](javascript:alert(1))\n\n" +
		"```go\n[[not/a/link/here]]\n```\n"
	got, err := Render(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script", "onerror", "javascript:"} {
		if strings.Contains(got.HTML, bad) {
			t.Fatalf("sanitizer let %q through: %s", bad, got.HTML)
		}
	}
	// goldmark percent-encodes non-ASCII path segments in href.
	encoded := "/wiki/" + url.PathEscape("诉讼") + "/" + url.PathEscape("股权纠纷") + "/" + url.PathEscape("裁判观点") + "/" + url.PathEscape("回购条款")
	if !strings.Contains(got.HTML, `href="`+encoded+`"`) {
		t.Fatalf("wiki link not rewritten: %s", got.HTML)
	}
	if !strings.Contains(got.HTML, `rel="nofollow noreferrer noopener"`) || !strings.Contains(got.HTML, `target="_blank"`) {
		t.Fatalf("external link policy missing: %s", got.HTML)
	}
	if len(got.Links) != 1 || got.Links[0] != "诉讼/股权纠纷/裁判观点/回购条款" {
		t.Fatalf("links must be distinct page paths: %v", got.Links)
	}
	if !strings.Contains(got.HTML, "[[not/a/link/here]]") {
		t.Fatalf("fenced code must stay literal: %s", got.HTML)
	}
	if strings.Contains(got.Text, "<") || !strings.Contains(got.Text, "要点") {
		t.Fatalf("plain text: %q", got.Text)
	}
}

func TestInvalidWikiTargetsStayLiteral(t *testing.T) {
	got, err := Render("[[too/short]] [[a/b/c/../x]]")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Links) != 0 || !strings.Contains(got.HTML, "[[too/short]]") {
		t.Fatalf("invalid targets must not become links: %v %s", got.Links, got.HTML)
	}
}

func TestSlugsAndPaths(t *testing.T) {
	for slug, ok := range map[string]bool{
		"诉讼": true, "equity-buyback": true, "v1.2": true, "a_b": true,
		"": false, ".hidden": false, "a/b": false, "a b": false, strings.Repeat("长", 81): false,
	} {
		if ValidSlug(slug) != ok {
			t.Errorf("slug %q: want %v", slug, ok)
		}
	}
	if NormalizePath(" /诉讼/Equity/Ch/Page/ ") != "诉讼/equity/ch/page" {
		t.Fatalf("normalize: %q", NormalizePath(" /诉讼/Equity/Ch/Page/ "))
	}
	if ValidPath("a/b/c") || !ValidPath("a/b/c/d") || ValidPath("a/b/../d") {
		t.Fatal("path depth or segment rules broken")
	}
}
