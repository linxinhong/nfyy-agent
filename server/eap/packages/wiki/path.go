package wiki

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Levels of the fixed tree; a page always sits under a chapter, so a full
// page path has exactly PathDepth segments.
const (
	LevelShelf   = "shelf"
	LevelBook    = "book"
	LevelChapter = "chapter"
	PathDepth    = 4
	maxSlugRunes = 80
)

// ParentLevel is the level a node's parent must have ("" for a shelf).
var ParentLevel = map[string]string{LevelShelf: "", LevelBook: LevelShelf, LevelChapter: LevelBook}

// NormalizeSlug trims and lowercases ASCII letters; CJK and other letters stay as written.
func NormalizeSlug(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r < utf8.RuneSelf {
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ValidSlug accepts 1-80 letters, digits, '-', '_' or '.', not starting with '.'.
// Letters include CJK, so 诉讼/股权纠纷 is a valid path.
func ValidSlug(s string) bool {
	n := utf8.RuneCountInString(s)
	if n == 0 || n > maxSlugRunes || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// NormalizePath normalizes each segment of a slash-separated path.
func NormalizePath(p string) string {
	segments := strings.Split(strings.Trim(strings.TrimSpace(p), "/"), "/")
	for i, s := range segments {
		segments[i] = NormalizeSlug(s)
	}
	return strings.Join(segments, "/")
}

// ValidPath is a full page path: shelf/book/chapter/page with valid slugs.
func ValidPath(p string) bool {
	segments := strings.Split(p, "/")
	if len(segments) != PathDepth {
		return false
	}
	for _, s := range segments {
		if !ValidSlug(s) {
			return false
		}
	}
	return true
}
