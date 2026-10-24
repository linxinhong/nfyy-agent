package dlp

// Max-match derivation (TASK-039): the streaming sanitizer may only hold
// back a bounded window, so it needs the length of the longest possible
// rule match. Deriving it from the compiled rule set (instead of a magic
// number) is what guarantees no match can straddle an SSE release
// boundary. Patterns whose match length cannot be bounded (unbounded
// repetition like `+`/`*` over non-empty content) report bounded=false;
// the streaming surface applies its documented cap in that case.

import (
	"regexp/syntax"
	"unicode/utf8"
)

// ruleMaxMatch returns the maximum byte length a match of the pattern can
// span, and whether that bound is finite. A parse failure reports
// unbounded (the safe direction).
func ruleMaxMatch(pattern string) (int, bool) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return 0, false
	}
	return reMaxBytes(re)
}

func reMaxBytes(re *syntax.Regexp) (int, bool) {
	switch re.Op {
	case syntax.OpLiteral:
		n := 0
		for _, r := range re.Rune {
			n += utf8.RuneLen(r)
		}
		return n, true
	case syntax.OpCharClass, syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return 1, true
	case syntax.OpEmptyMatch,
		syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return 0, true
	case syntax.OpCapture:
		return reMaxBytes(re.Sub[0])
	case syntax.OpConcat:
		n := 0
		for _, sub := range re.Sub {
			m, ok := reMaxBytes(sub)
			if !ok {
				return 0, false
			}
			n += m
		}
		return n, true
	case syntax.OpAlternate:
		max, ok := 0, true
		for _, sub := range re.Sub {
			m, subOK := reMaxBytes(sub)
			if !subOK {
				return 0, false
			}
			if m > max {
				max = m
			}
		}
		return max, ok
	case syntax.OpQuest:
		m, ok := reMaxBytes(re.Sub[0])
		return m, ok
	case syntax.OpRepeat:
		m, ok := reMaxBytes(re.Sub[0])
		if !ok || m == 0 {
			return 0, ok || m == 0
		}
		if re.Max < 0 {
			return 0, false // unbounded repetition of a non-empty body
		}
		return m * re.Max, true
	case syntax.OpStar, syntax.OpPlus:
		m, ok := reMaxBytes(re.Sub[0])
		if !ok || m > 0 {
			return 0, false // unbounded repetition of a possibly-non-empty body
		}
		return 0, true
	default:
		return 0, false
	}
}
