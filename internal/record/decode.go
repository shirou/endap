package record

import (
	"errors"
	"fmt"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrMalformed is returned for any line that is not a well-formed record.
var ErrMalformed = errors.New("malformed record")

// maxDepth is the nesting limit, counted in open containers. It is the same
// limit encoding/json enforces, and it has to be the same: the differential
// test holds this parser and the standard library to identical verdicts, so a
// deeper limit here would show up as a fuzz failure rather than as leniency.
// It also keeps a pathological line from recursing until the stack gives out.
const maxDepth = 10000

// Decode parses one JSONL line into dst.
//
// This is the single swap boundary of D4: the whole hand-rolled parser sits
// behind this signature, and decode_std_test.go keeps an encoding/json
// implementation of the same signature as the reference for the differential
// test, so replacing this file later stays a local change.
//
// It is a structural scan of a flat JSON object rather than a substring hunt
// for "cmd": and friends. Values that are not strings or numbers are skipped,
// so a future nested field does not turn every older record into a parse error.
func Decode(line []byte, dst *Record) error {
	*dst = Record{}
	p := parser{b: line}
	if err := p.object(dst); err != nil {
		return err
	}
	if p.bad != 0 {
		return fmt.Errorf("%w: field has a value of the wrong type", ErrMalformed)
	}
	p.ws()
	if p.i != len(p.b) {
		return fmt.Errorf("%w: trailing bytes at offset %d", ErrMalformed, p.i)
	}
	return nil
}

type parser struct {
	b []byte
	i int
	// bad marks known fields whose most recent value was well-formed JSON of
	// the wrong type. See field.
	bad uint16
}

func (p *parser) errf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrMalformed}, args...)...)
}

func (p *parser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) object(dst *Record) error {
	p.ws()
	if p.i >= len(p.b) || p.b[p.i] != '{' {
		return p.errf("object expected")
	}
	p.i++
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return nil
	}
	for {
		p.ws()
		key, err := p.str()
		if err != nil {
			return err
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return p.errf("':' expected after key")
		}
		p.i++
		p.ws()
		if err := p.field(key, dst); err != nil {
			return err
		}
		p.ws()
		if p.i >= len(p.b) {
			return p.errf("unterminated object")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return nil
		default:
			return p.errf("',' or '}' expected")
		}
	}
}

// null consumes a null literal if one is next, reporting whether it did.
func (p *parser) null() bool {
	if p.i+4 <= len(p.b) && string(p.b[p.i:p.i+4]) == "null" {
		p.i += 4
		return true
	}
	return false
}

// fieldIndex numbers the known keys so field can remember, per key, whether the
// last value seen for it had the wrong type.
func fieldIndex(key []byte) (int, bool) {
	switch string(key) {
	case "v":
		return 0, true
	case "id":
		return 1, true
	case "ts":
		return 2, true
	case "dur":
		return 3, true
	case "cmd":
		return 4, true
	case "cwd":
		return 5, true
	case "host":
		return 6, true
	case "sess":
		return 7, true
	case "exit":
		return 8, true
	case "sh":
		return 9, true
	}
	return 0, false
}

// field parses the value of one key into dst.
//
// A value of the wrong type is not a parse error on its own. JSON allows a key
// to repeat, and both this parser and encoding/json take the last occurrence, so
// {"cmd":0,"cmd":"ls"} is a valid record whose cmd is "ls". The mismatch is
// recorded against the key instead, cleared if a later occurrence is usable, and
// turned into an error at the end of Decode if it was not. Anything that is not
// well-formed JSON still fails here and now.
func (p *parser) field(key []byte, dst *Record) error {
	idx, known := fieldIndex(key)
	if !known {
		return p.skip(1)
	}
	start := p.i
	err := p.value(idx, dst)
	if err == nil {
		p.bad &^= 1 << idx
		return nil
	}
	// Rewind and see whether this was merely the wrong type.
	p.i = start
	if skipErr := p.skip(1); skipErr != nil {
		return err
	}
	p.bad |= 1 << idx
	return nil
}

func (p *parser) value(idx int, dst *Record) error {
	switch idx {
	case 0:
		n, err := p.optNum()
		if err != nil {
			return err
		}
		dst.V = int(n)
	case 1:
		return p.strInto(&dst.ID)
	case 2:
		n, err := p.optNum()
		if err != nil {
			return err
		}
		dst.TS = n
	case 3:
		if p.null() {
			dst.Dur, dst.HasDur = 0, false
			return nil
		}
		n, err := p.intVal()
		if err != nil {
			return err
		}
		dst.SetDur(n)
	case 4:
		return p.strInto(&dst.Cmd)
	case 5:
		return p.strInto(&dst.Cwd)
	case 6:
		return p.strInto(&dst.Host)
	case 7:
		return p.strInto(&dst.Sess)
	case 8:
		if p.null() {
			dst.Exit, dst.HasExit = 0, false
			return nil
		}
		n, err := p.intVal()
		if err != nil {
			return err
		}
		dst.SetExit(int(n))
	case 9:
		return p.strInto(&dst.Sh)
	}
	return nil
}

func (p *parser) optNum() (int64, error) {
	if p.null() {
		return 0, nil
	}
	return p.intVal()
}

func (p *parser) strInto(dst *string) error {
	if p.null() {
		// A key may repeat and the last value wins, so an explicit null has to
		// clear whatever an earlier occurrence of the same key set.
		*dst = ""
		return nil
	}
	s, err := p.str()
	if err != nil {
		return err
	}
	*dst = string(s)
	return nil
}

// str reads a JSON string. The returned bytes alias p.b when the literal held
// no escape sequence, so the common case allocates nothing beyond the final
// string conversion.
//
// Termination is decided by walking escapes rather than by looking back one
// byte: a backslash always consumes exactly the byte after it, which makes a
// value ending in a backslash ("echo \\") fall out correctly instead of
// swallowing the closing quote.
func (p *parser) str() ([]byte, error) {
	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return nil, p.errf("string expected")
	}
	p.i++
	start := p.i
	escaped := false
	// Most history is ASCII, and ASCII is always valid UTF-8. Noticing that
	// during the scan the parser is already doing saves a second pass over
	// every string.
	ascii := true
	for p.i < len(p.b) {
		switch c := p.b[p.i]; {
		case c == '"':
			raw := p.b[start:p.i]
			p.i++
			if !escaped {
				if ascii {
					return raw, nil
				}
				return coerceUTF8(raw), nil
			}
			out, err := unescape(raw)
			if err != nil {
				return nil, err
			}
			return coerceUTF8(out), nil
		case c == '\\':
			escaped = true
			p.i++
			if p.i >= len(p.b) {
				return nil, p.errf("unterminated escape")
			}
			if p.b[p.i] >= 0x80 {
				ascii = false
			}
		case c < 0x20:
			return nil, p.errf("control character in string")
		case c >= 0x80:
			ascii = false
		}
		p.i++
	}
	return nil, p.errf("unterminated string")
}

// coerceUTF8 replaces each invalid UTF-8 byte with U+FFFD, which is what
// encoding/json does when it unquotes a string. The check is a plain scan and
// the common case returns the input untouched.
func coerceUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	out := make([]byte, 0, len(b)+utf8.UTFMax)
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			out = utf8.AppendRune(out, utf8.RuneError)
			i++
			continue
		}
		out = append(out, b[i:i+size]...)
		i += size
	}
	return out
}

func hex4(s []byte) (rune, bool) {
	var r rune
	for _, c := range s {
		var d rune
		switch {
		case c >= '0' && c <= '9':
			d = rune(c - '0')
		case c >= 'a' && c <= 'f':
			d = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = rune(c-'A') + 10
		default:
			return 0, false
		}
		r = r<<4 | d
	}
	return r, true
}

// unescape decodes the escape sequences encoding/json can emit. \uXXXX handling
// is not optional even with SetEscapeHTML(false): U+2028 and U+2029 are always
// escaped, and so is every control character below 0x20.
func unescape(s []byte) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		i++
		if i >= len(s) {
			return nil, fmt.Errorf("%w: dangling escape", ErrMalformed)
		}
		switch s[i] {
		case '"':
			out = append(out, '"')
		case '\\':
			out = append(out, '\\')
		case '/':
			out = append(out, '/')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			if i+5 > len(s) {
				return nil, fmt.Errorf("%w: short \\u escape", ErrMalformed)
			}
			r, ok := hex4(s[i+1 : i+5])
			if !ok {
				return nil, fmt.Errorf("%w: bad \\u escape", ErrMalformed)
			}
			i += 5
			if utf16.IsSurrogate(r) {
				if i+6 <= len(s) && s[i] == '\\' && s[i+1] == 'u' {
					if r2, ok := hex4(s[i+2 : i+6]); ok {
						if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
							out = utf8.AppendRune(out, dec)
							i += 6
							continue
						}
					}
				}
				// Unpaired surrogate: encoding/json substitutes U+FFFD here.
				out = utf8.AppendRune(out, utf8.RuneError)
				continue
			}
			out = utf8.AppendRune(out, r)
			continue
		default:
			return nil, fmt.Errorf("%w: unknown escape \\%c", ErrMalformed, s[i])
		}
		i++
	}
	return out, nil
}

// intVal parses a JSON number into an int64. Every numeric field of a record is
// an integer, so a fraction or exponent is rejected here exactly as
// encoding/json rejects it when the destination field is an int.
func (p *parser) intVal() (int64, error) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	digits := p.i
	if err := p.intPart(); err != nil {
		return 0, err
	}
	if p.i < len(p.b) {
		switch p.b[p.i] {
		case '.', 'e', 'E':
			return 0, p.errf("integer expected")
		}
	}
	if n := p.i - digits; n <= 18 {
		var v int64
		for _, c := range p.b[digits:p.i] {
			v = v*10 + int64(c-'0')
		}
		if p.b[start] == '-' {
			v = -v
		}
		return v, nil
	}
	v, err := strconv.ParseInt(string(p.b[start:p.i]), 10, 64)
	if err != nil {
		return 0, p.errf("number out of range")
	}
	return v, nil
}

// intPart consumes the JSON integer part: a lone 0, or a non-zero digit run.
func (p *parser) intPart() error {
	if p.i >= len(p.b) {
		return p.errf("number expected")
	}
	switch c := p.b[p.i]; {
	case c == '0':
		p.i++
	case c >= '1' && c <= '9':
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
	default:
		return p.errf("number expected")
	}
	return nil
}

// numJSON validates and consumes any JSON number. Only unknown fields reach it,
// so the value is discarded; the point is to reject syntax encoding/json would
// also reject, such as a leading zero.
func (p *parser) numJSON() error {
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	if err := p.intPart(); err != nil {
		return err
	}
	if p.i < len(p.b) && p.b[p.i] == '.' {
		p.i++
		if err := p.digits(); err != nil {
			return err
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		if err := p.digits(); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) digits() error {
	start := p.i
	for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
		p.i++
	}
	if p.i == start {
		return p.errf("digit expected")
	}
	return nil
}

// skip consumes one value of any type, so unknown fields never fail a record.
//
// depth is how many containers are already open around this value.
func (p *parser) skip(depth int) error {
	p.ws()
	if p.i >= len(p.b) {
		return p.errf("value expected")
	}
	switch c := p.b[p.i]; {
	case c == '"':
		_, err := p.str()
		return err
	case c == '{':
		if depth++; depth > maxDepth {
			return p.errf("exceeded max depth")
		}
		p.i++
		p.ws()
		if p.i < len(p.b) && p.b[p.i] == '}' {
			p.i++
			return nil
		}
		for {
			p.ws()
			if _, err := p.str(); err != nil {
				return err
			}
			p.ws()
			if p.i >= len(p.b) || p.b[p.i] != ':' {
				return p.errf("':' expected")
			}
			p.i++
			if err := p.skip(depth); err != nil {
				return err
			}
			p.ws()
			if p.i >= len(p.b) {
				return p.errf("unterminated object")
			}
			switch p.b[p.i] {
			case ',':
				p.i++
			case '}':
				p.i++
				return nil
			default:
				return p.errf("',' or '}' expected")
			}
		}
	case c == '[':
		if depth++; depth > maxDepth {
			return p.errf("exceeded max depth")
		}
		p.i++
		p.ws()
		if p.i < len(p.b) && p.b[p.i] == ']' {
			p.i++
			return nil
		}
		for {
			if err := p.skip(depth); err != nil {
				return err
			}
			p.ws()
			if p.i >= len(p.b) {
				return p.errf("unterminated array")
			}
			switch p.b[p.i] {
			case ',':
				p.i++
			case ']':
				p.i++
				return nil
			default:
				return p.errf("',' or ']' expected")
			}
		}
	case c == 't':
		return p.lit("true")
	case c == 'f':
		return p.lit("false")
	case c == 'n':
		return p.lit("null")
	default:
		return p.numJSON()
	}
}

func (p *parser) lit(want string) error {
	if p.i+len(want) > len(p.b) || string(p.b[p.i:p.i+len(want)]) != want {
		return p.errf("%s expected", want)
	}
	p.i += len(want)
	return nil
}
