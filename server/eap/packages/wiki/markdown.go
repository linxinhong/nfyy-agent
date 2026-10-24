// Package wiki holds the knowledge layer's pure rules: Markdown rendering and
// sanitizing, internal [[links]], slugs and page paths, plus the node tree
// store (nodes.go).
//
// Why: Markdown is the canonical page format because agents write it; the
// server renders and sanitizes it so the website and desktop only ever show
// HTML that passed one policy.
//
// Invariants and pitfalls:
//   - Never serve HTML that did not go through Render: the sanitizer is the
//     only XSS boundary (agents and members both author Markdown).
//   - [[target|label]] is rewritten outside fenced code only; inline code
//     spans are left alone by the renderer, so a [[...]] there stays literal.
//   - Internal links render as /wiki/<path>; whether the reader may open the
//     target is decided at read time, never baked into stored text.
package wiki

import (
	"bytes"
	"html"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// LinkPrefix is the site path of a page; links are LinkPrefix + page path.
const LinkPrefix = "/wiki/"

var (
	markdown = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(gmhtml.WithHardWraps()),
	)
	policy       = newPolicy()
	textPolicy   = bluemonday.StrictPolicy()
	wikiLink     = regexp.MustCompile(`\[\[([^\[\]|\n]+)(?:\|([^\[\]\n]+))?\]\]`)
	fence        = regexp.MustCompile("^\\s{0,3}(```|~~~)")
	spaces       = regexp.MustCompile(`\s+`)
	codeLanguage = regexp.MustCompile(`^language-[A-Za-z0-9_+-]{1,32}$`)
	markdownEsc  = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)
)

func newPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("class").Matching(codeLanguage).OnElements("code")
	p.RequireNoFollowOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.RequireNoReferrerOnFullyQualifiedLinks(true)
	return p
}

// Rendered is one Markdown body prepared for storage and display.
type Rendered struct {
	HTML  string   // sanitized
	Text  string   // plain text for search and snippets
	Links []string // distinct internal page paths, in order of appearance
}

// rewriteWikiLinks turns [[path|label]] into Markdown links outside fenced code.
func rewriteWikiLinks(source string) (string, []string) {
	var out strings.Builder
	var links []string
	seen := map[string]bool{}
	inFence := ""
	for _, line := range strings.SplitAfter(source, "\n") {
		if m := fence.FindStringSubmatch(line); m != nil {
			switch {
			case inFence == "":
				inFence = m[1]
			case inFence == m[1]:
				inFence = ""
			}
			out.WriteString(line)
			continue
		}
		if inFence != "" {
			out.WriteString(line)
			continue
		}
		out.WriteString(wikiLink.ReplaceAllStringFunc(line, func(match string) string {
			parts := wikiLink.FindStringSubmatch(match)
			target, label := NormalizePath(parts[1]), strings.TrimSpace(parts[2])
			if !ValidPath(target) {
				return match
			}
			if label == "" {
				label = target[strings.LastIndex(target, "/")+1:]
			}
			if !seen[target] {
				seen[target] = true
				links = append(links, target)
			}
			return "[" + markdownEsc.Replace(label) + "](" + LinkPrefix + escapePath(target) + ")"
		}))
	}
	return out.String(), links
}

func escapePath(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29").Replace(s)
	}
	return strings.Join(segments, "/")
}

// Render converts Markdown to sanitized HTML, plain text and internal links.
func Render(source string) (Rendered, error) {
	rewritten, links := rewriteWikiLinks(source)
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(rewritten), &buf); err != nil {
		return Rendered{}, err
	}
	safe := policy.SanitizeBytes(buf.Bytes())
	text := html.UnescapeString(textPolicy.Sanitize(strings.NewReplacer("<", " <", ">", "> ").Replace(string(safe))))
	if links == nil {
		links = []string{}
	}
	return Rendered{HTML: string(safe), Text: strings.TrimSpace(spaces.ReplaceAllString(text, " ")), Links: links}, nil
}
