// Package query implements the matchmaker ticket criteria language: a small
// boolean expression grammar over string and numeric properties, parsed to
// an AST and evaluated in Go against candidate tickets. User input is never
// compiled to SQL.
//
// Grammar:
//
//	expr    := or
//	or      := and ("OR" and)*
//	and     := unary ("AND" unary)*
//	unary   := "NOT" unary | primary
//	primary := "(" expr ")" | "*" | term
//	term    := key ":" string | key op number
//	op      := "=" | "!=" | ">" | ">=" | "<" | "<="
//
// Keys match ^[a-z0-9_]+$. String values are bare words ([A-Za-z0-9_.-]+)
// or double-quoted. A term on a missing property is false.
package query

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLength caps accepted query strings.
const MaxLength = 512

// Props is the property view of one ticket that expressions evaluate
// against.
type Props struct {
	Strings map[string]string
	Numbers map[string]float64
}

// Expr is a parsed query expression.
type Expr interface {
	Eval(p Props) bool
}

type matchAll struct{}

func (matchAll) Eval(Props) bool { return true }

type notExpr struct{ inner Expr }

func (e notExpr) Eval(p Props) bool { return !e.inner.Eval(p) }

type andExpr struct{ left, right Expr }

func (e andExpr) Eval(p Props) bool { return e.left.Eval(p) && e.right.Eval(p) }

type orExpr struct{ left, right Expr }

func (e orExpr) Eval(p Props) bool { return e.left.Eval(p) || e.right.Eval(p) }

type strTerm struct {
	key, value string
}

func (e strTerm) Eval(p Props) bool {
	v, ok := p.Strings[e.key]
	return ok && v == e.value
}

type numTerm struct {
	key   string
	op    string
	value float64
}

func (e numTerm) Eval(p Props) bool {
	v, ok := p.Numbers[e.key]
	if !ok {
		return false
	}
	switch e.op {
	case "=":
		return v == e.value
	case "!=":
		return v != e.value
	case ">":
		return v > e.value
	case ">=":
		return v >= e.value
	case "<":
		return v < e.value
	case "<=":
		return v <= e.value
	}
	return false
}

// MatchAll is the expression the empty / "*" query parses to.
var MatchAll Expr = matchAll{}

// Parse compiles a query string. Empty and "*" match everything.
func Parse(input string) (Expr, error) {
	if len(input) > MaxLength {
		return nil, fmt.Errorf("query: longer than %d bytes", MaxLength)
	}
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || trimmed == "*" {
		return MatchAll, nil
	}
	toks, err := lex(trimmed)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	expr, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if !p.eof() {
		return nil, fmt.Errorf("query: unexpected %q", p.peek().text)
	}
	return expr, nil
}

type tokenKind int

const (
	tokLParen tokenKind = iota
	tokRParen
	tokStar
	tokAnd
	tokOr
	tokNot
	tokColon
	tokOp
	tokWord   // bare word: key or unquoted string value
	tokString // quoted string value
	tokNumber
)

type token struct {
	kind tokenKind
	text string
}

func lex(s string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c < utf8.RuneSelf && unicode.IsSpace(rune(c)):
			// Whitespace is ASCII-only. c is a byte, so guard against
			// casting a multi-byte UTF-8 lead/continuation byte to a rune
			// (e.g. 0xA0/0x85 would otherwise be misread as space).
			i++
		case c == '(':
			toks = append(toks, token{tokLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tokRParen, ")"})
			i++
		case c == '*':
			toks = append(toks, token{tokStar, "*"})
			i++
		case c == ':':
			toks = append(toks, token{tokColon, ":"})
			i++
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				j++
			}
			if j == len(s) {
				return nil, fmt.Errorf("query: unterminated string")
			}
			toks = append(toks, token{tokString, s[i+1 : j]})
			i = j + 1
		case c == '=', c == '!', c == '>', c == '<':
			j := i + 1
			if j < len(s) && s[j] == '=' {
				j++
			}
			op := s[i:j]
			if op == "!" {
				return nil, fmt.Errorf("query: bare '!' (use !=)")
			}
			toks = append(toks, token{tokOp, op})
			i = j
		case isWordChar(c) || c == '-':
			j := i
			for j < len(s) && (isWordChar(s[j]) || s[j] == '-') {
				j++
			}
			word := s[i:j]
			switch strings.ToUpper(word) {
			case "AND":
				toks = append(toks, token{tokAnd, word})
			case "OR":
				toks = append(toks, token{tokOr, word})
			case "NOT":
				toks = append(toks, token{tokNot, word})
			default:
				if _, err := strconv.ParseFloat(word, 64); err == nil {
					toks = append(toks, token{tokNumber, word})
				} else {
					toks = append(toks, token{tokWord, word})
				}
			}
			i = j
		default:
			return nil, fmt.Errorf("query: unexpected character %q", string(c))
		}
	}
	return toks, nil
}

func isWordChar(c byte) bool {
	return c == '_' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) eof() bool   { return p.pos >= len(p.toks) }
func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) accept(k tokenKind) bool {
	if !p.eof() && p.toks[p.pos].kind == k {
		p.pos++
		return true
	}
	return false
}

func (p *parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.accept(tokOr) {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = orExpr{left, right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.accept(tokAnd) {
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = andExpr{left, right}
	}
	return left, nil
}

func (p *parser) parseUnary() (Expr, error) {
	if p.accept(tokNot) {
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return notExpr{inner}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	if p.eof() {
		return nil, fmt.Errorf("query: unexpected end of input")
	}
	switch p.peek().kind {
	case tokLParen:
		p.next()
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.accept(tokRParen) {
			return nil, fmt.Errorf("query: missing ')'")
		}
		return inner, nil
	case tokStar:
		p.next()
		return MatchAll, nil
	case tokWord:
		return p.parseTerm()
	default:
		return nil, fmt.Errorf("query: unexpected %q", p.peek().text)
	}
}

func (p *parser) parseTerm() (Expr, error) {
	key := p.next().text
	if !ValidKey(key) {
		return nil, fmt.Errorf("query: invalid property key %q", key)
	}
	if p.eof() {
		return nil, fmt.Errorf("query: expected ':' or comparison after %q", key)
	}
	switch tok := p.next(); tok.kind {
	case tokColon:
		if p.eof() {
			return nil, fmt.Errorf("query: expected string value for %q", key)
		}
		val := p.next()
		if val.kind != tokWord && val.kind != tokString && val.kind != tokNumber {
			return nil, fmt.Errorf("query: expected string value for %q, got %q", key, val.text)
		}
		return strTerm{key: key, value: val.text}, nil
	case tokOp:
		if p.eof() || p.peek().kind != tokNumber {
			return nil, fmt.Errorf("query: expected number after %q %s", key, tok.text)
		}
		num, err := strconv.ParseFloat(p.next().text, 64)
		if err != nil {
			return nil, fmt.Errorf("query: bad number for %q: %w", key, err)
		}
		return numTerm{key: key, op: tok.text, value: num}, nil
	default:
		return nil, fmt.Errorf("query: expected ':' or comparison after %q, got %q", key, tok.text)
	}
}

// ValidKey reports whether s is a legal property key: ^[a-z0-9_]{1,64}$ that
// the lexer would not read as a number. A key the lexer tokenises as a number
// (all-digit like "123", scientific "1e5", or "inf"/"nan") can never appear on
// the left of a term, so it would be settable as a property but permanently
// non-queryable — reject it so every settable key is also queryable.
func ValidKey(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return false
	}
	return true
}
