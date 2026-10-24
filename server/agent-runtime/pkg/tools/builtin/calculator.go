// Package builtin provides built-in tools available to agents, teams, and workflows.
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// Calculator evaluates a safe arithmetic expression and returns the result as
// a JSON string. Only basic arithmetic operators, parentheses, numbers and
// whitespace are allowed.
func Calculator(ctx context.Context, expression string) (string, error) {
	result, err := eval(expression)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(map[string]any{
		"expression": expression,
		"result":     result,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// NewCalculatorTool returns a Tool wrapping Calculator.
func NewCalculatorTool() (*tools.Function, error) {
	return tools.NewFunction("calculator", "Evaluate a safe arithmetic expression.", Calculator)
}

// eval parses and evaluates a mathematical expression safely.
func eval(expr string) (float64, error) {
	p := &parser{tokens: tokenize(expr)}
	p.next()
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	if p.tok.typ != tokenEOF {
		return 0, fmt.Errorf("unexpected token %q at position %d", p.tok.val, p.tok.pos)
	}
	return v, nil
}

type tokenType int

const (
	tokenEOF tokenType = iota
	tokenNumber
	tokenPlus
	tokenMinus
	tokenMul
	tokenDiv
	tokenPow
	tokenLParen
	tokenRParen
)

type token struct {
	typ tokenType
	val string
	pos int
}

func tokenize(s string) []token {
	var tokens []token
	for i := 0; i < len(s); {
		c := s[i]
		if unicode.IsSpace(rune(c)) {
			i++
			continue
		}
		switch c {
		case '+':
			tokens = append(tokens, token{typ: tokenPlus, val: string(c), pos: i})
			i++
		case '-':
			tokens = append(tokens, token{typ: tokenMinus, val: string(c), pos: i})
			i++
		case '*':
			tokens = append(tokens, token{typ: tokenMul, val: string(c), pos: i})
			i++
		case '/':
			tokens = append(tokens, token{typ: tokenDiv, val: string(c), pos: i})
			i++
		case '^':
			tokens = append(tokens, token{typ: tokenPow, val: string(c), pos: i})
			i++
		case '(':
			tokens = append(tokens, token{typ: tokenLParen, val: string(c), pos: i})
			i++
		case ')':
			tokens = append(tokens, token{typ: tokenRParen, val: string(c), pos: i})
			i++
		default:
			if c >= '0' && c <= '9' || c == '.' {
				start := i
				dotCount := 0
				for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
					if s[i] == '.' {
						dotCount++
					}
					i++
				}
				val := s[start:i]
				if dotCount > 1 {
					tokens = append(tokens, token{typ: tokenNumber, val: val, pos: start})
				} else {
					tokens = append(tokens, token{typ: tokenNumber, val: val, pos: start})
				}
			} else {
				// Unknown characters are collected as a single token; the parser
				// will fail with a descriptive error.
				start := i
				for i < len(s) && !unicode.IsSpace(rune(s[i])) && !strings.ContainsRune("+-*/^()", rune(s[i])) {
					i++
				}
				tokens = append(tokens, token{typ: tokenNumber, val: s[start:i], pos: start})
			}
		}
	}
	tokens = append(tokens, token{typ: tokenEOF, pos: len(s)})
	return tokens
}

type parser struct {
	tokens []token
	pos    int
	tok    token
}

func (p *parser) next() {
	if p.pos < len(p.tokens) {
		p.tok = p.tokens[p.pos]
	} else {
		p.tok = token{typ: tokenEOF}
	}
	p.pos++
}

func (p *parser) parseExpr() (float64, error) {
	v, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for p.tok.typ == tokenPlus || p.tok.typ == tokenMinus {
		op := p.tok.typ
		p.next()
		rhs, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if op == tokenPlus {
			v += rhs
		} else {
			v -= rhs
		}
	}
	return v, nil
}

func (p *parser) parseTerm() (float64, error) {
	v, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for p.tok.typ == tokenMul || p.tok.typ == tokenDiv {
		op := p.tok.typ
		p.next()
		rhs, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		if op == tokenMul {
			v *= rhs
		} else {
			if rhs == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			v /= rhs
		}
	}
	return v, nil
}

func (p *parser) parseFactor() (float64, error) {
	v, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	if p.tok.typ == tokenPow {
		p.next()
		rhs, err := p.parseFactor() // right-associative
		if err != nil {
			return 0, err
		}
		v = math.Pow(v, rhs)
	}
	return v, nil
}

func (p *parser) parseUnary() (float64, error) {
	if p.tok.typ == tokenMinus {
		p.next()
		v, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		return -v, nil
	}
	if p.tok.typ == tokenPlus {
		p.next()
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (float64, error) {
	switch p.tok.typ {
	case tokenNumber:
		v, err := strconv.ParseFloat(p.tok.val, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid number %q at position %d", p.tok.val, p.tok.pos)
		}
		p.next()
		return v, nil
	case tokenLParen:
		p.next()
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if p.tok.typ != tokenRParen {
			return 0, fmt.Errorf("expected ')' at position %d", p.tok.pos)
		}
		p.next()
		return v, nil
	default:
		return 0, fmt.Errorf("unexpected token %q at position %d", p.tok.val, p.tok.pos)
	}
}
