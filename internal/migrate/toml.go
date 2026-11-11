package migrate

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// decodeTOML reads the TOML a config file uses into maps, lists, strings,
// int64, float64 and bool: tables and arrays of tables, dotted and quoted
// keys, basic, literal and multi-line strings, numbers, booleans, arrays
// and inline tables. Dates come back as strings. It is enough for the
// configs the import reads, not a full TOML implementation.
func decodeTOML(data []byte) (map[string]any, error) {
	p := &tomlParser{s: strings.ReplaceAll(string(data), "\r\n", "\n"), line: 1}
	root := map[string]any{}
	cur := root
	for {
		p.skipSpaceAndComments(true)
		if p.eof() {
			return root, nil
		}
		switch {
		case strings.HasPrefix(p.rest(), "[["):
			p.pos += 2
			keys, err := p.keyPath()
			if err != nil {
				return nil, err
			}
			if !p.consume("]]") {
				return nil, p.errf("want ]] after the table name")
			}
			parent, err := tableAt(root, keys[:len(keys)-1])
			if err != nil {
				return nil, p.errf("%v", err)
			}
			last := keys[len(keys)-1]
			list, _ := parent[last].([]any)
			if _, ok := parent[last]; ok && list == nil {
				return nil, p.errf("%s is not an array of tables", strings.Join(keys, "."))
			}
			cur = map[string]any{}
			parent[last] = append(list, cur)
		case p.peek() == '[':
			p.pos++
			keys, err := p.keyPath()
			if err != nil {
				return nil, err
			}
			if !p.consume("]") {
				return nil, p.errf("want ] after the table name")
			}
			if cur, err = tableAt(root, keys); err != nil {
				return nil, p.errf("%v", err)
			}
		default:
			if err := p.keyValue(cur); err != nil {
				return nil, err
			}
		}
		p.skipSpace()
		if p.peek() == '#' {
			p.skipComment()
		}
		if !p.eof() && !p.consume("\n") {
			return nil, p.errf("unexpected %q", p.peek())
		}
		p.line++
	}
}

// tableAt walks to (creating) the table at keys; the last element of an
// array of tables stands for the array.
func tableAt(root map[string]any, keys []string) (map[string]any, error) {
	t := root
	for i, k := range keys {
		switch v := t[k].(type) {
		case nil:
			n := map[string]any{}
			t[k] = n
			t = n
		case map[string]any:
			t = v
		case []any:
			if len(v) == 0 {
				return nil, fmt.Errorf("%s is an empty array", strings.Join(keys[:i+1], "."))
			}
			last, ok := v[len(v)-1].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s is not a table", strings.Join(keys[:i+1], "."))
			}
			t = last
		default:
			return nil, fmt.Errorf("%s is a value, not a table", strings.Join(keys[:i+1], "."))
		}
	}
	return t, nil
}

type tomlParser struct {
	s    string
	pos  int
	line int
}

func (p *tomlParser) eof() bool    { return p.pos >= len(p.s) }
func (p *tomlParser) rest() string { return p.s[p.pos:] }
func (p *tomlParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.s[p.pos]
}

func (p *tomlParser) consume(tok string) bool {
	if strings.HasPrefix(p.rest(), tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

func (p *tomlParser) errf(format string, args ...any) error {
	return fmt.Errorf("line %d: %s", p.line, fmt.Sprintf(format, args...))
}

func (p *tomlParser) skipSpace() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.pos++
	}
}

func (p *tomlParser) skipComment() {
	for !p.eof() && p.peek() != '\n' {
		p.pos++
	}
}

// skipSpaceAndComments skips blanks and comments, and with newlines also
// line ends.
func (p *tomlParser) skipSpaceAndComments(newlines bool) {
	for !p.eof() {
		switch c := p.peek(); {
		case c == ' ' || c == '\t' || c == '\r':
			p.pos++
		case c == '#':
			p.skipComment()
		case c == '\n' && newlines:
			p.pos++
			p.line++
		default:
			return
		}
	}
}

// keyPath reads a dotted key: a.b."c d".'e'.
func (p *tomlParser) keyPath() ([]string, error) {
	var keys []string
	for {
		p.skipSpace()
		var k string
		switch p.peek() {
		case '"':
			s, err := p.basicString()
			if err != nil {
				return nil, err
			}
			k = s
		case '\'':
			s, err := p.literalString()
			if err != nil {
				return nil, err
			}
			k = s
		default:
			start := p.pos
			for !p.eof() {
				c := p.peek()
				if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
					p.pos++
					continue
				}
				break
			}
			if p.pos == start {
				return nil, p.errf("want a key")
			}
			k = p.s[start:p.pos]
		}
		keys = append(keys, k)
		p.skipSpace()
		if !p.consume(".") {
			return keys, nil
		}
	}
}

// keyValue reads "key = value" into t.
func (p *tomlParser) keyValue(t map[string]any) error {
	keys, err := p.keyPath()
	if err != nil {
		return err
	}
	if !p.consume("=") {
		return p.errf("want = after %s", strings.Join(keys, "."))
	}
	p.skipSpace()
	v, err := p.value()
	if err != nil {
		return err
	}
	parent, err := tableAt(t, keys[:len(keys)-1])
	if err != nil {
		return p.errf("%v", err)
	}
	parent[keys[len(keys)-1]] = v
	return nil
}

func (p *tomlParser) value() (any, error) {
	switch c := p.peek(); {
	case strings.HasPrefix(p.rest(), `"""`):
		return p.multiString(`"""`, true)
	case strings.HasPrefix(p.rest(), `'''`):
		return p.multiString(`'''`, false)
	case c == '"':
		return p.basicString()
	case c == '\'':
		return p.literalString()
	case c == '[':
		return p.array()
	case c == '{':
		return p.inlineTable()
	case p.consume("true"):
		return true, nil
	case p.consume("false"):
		return false, nil
	}
	start := p.pos
	for !p.eof() {
		c := p.peek()
		if c == ',' || c == ']' || c == '}' || c == '\n' || c == '#' {
			break
		}
		p.pos++
	}
	raw := strings.TrimSpace(p.s[start:p.pos])
	if raw == "" {
		return nil, p.errf("want a value")
	}
	n := strings.ReplaceAll(raw, "_", "")
	if i, err := strconv.ParseInt(n, 0, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(n, 64); err == nil {
		return f, nil
	}
	switch n {
	case "inf", "+inf", "-inf", "nan", "+nan", "-nan":
		f, _ := strconv.ParseFloat(n, 64)
		return f, nil
	}
	if raw[0] >= '0' && raw[0] <= '9' {
		return raw, nil // a date or time
	}
	return nil, p.errf("cannot read the value %q", raw)
}

func (p *tomlParser) basicString() (string, error) {
	p.pos++ // "
	var b strings.Builder
	for {
		if p.eof() || p.peek() == '\n' {
			return "", p.errf("unterminated string")
		}
		c := p.peek()
		switch c {
		case '"':
			p.pos++
			return b.String(), nil
		case '\\':
			if err := p.escape(&b); err != nil {
				return "", err
			}
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
}

func (p *tomlParser) escape(b *strings.Builder) error {
	p.pos++ // backslash
	if p.eof() {
		return p.errf("unterminated escape")
	}
	c := p.peek()
	p.pos++
	switch c {
	case 'n':
		b.WriteByte('\n')
	case 't':
		b.WriteByte('\t')
	case 'r':
		b.WriteByte('\r')
	case 'b':
		b.WriteByte('\b')
	case 'f':
		b.WriteByte('\f')
	case 'e':
		b.WriteByte(0x1b)
	case '"', '\\':
		b.WriteByte(c)
	case 'u', 'U':
		n := 4
		if c == 'U' {
			n = 8
		}
		if p.pos+n > len(p.s) {
			return p.errf("short unicode escape")
		}
		r, err := strconv.ParseUint(p.s[p.pos:p.pos+n], 16, 32)
		if err != nil || !utf8.ValidRune(rune(r)) {
			return p.errf("bad unicode escape")
		}
		b.WriteRune(rune(r))
		p.pos += n
	default:
		return p.errf("unknown escape \\%c", c)
	}
	return nil
}

func (p *tomlParser) literalString() (string, error) {
	p.pos++ // '
	end := strings.IndexAny(p.rest(), "'\n")
	if end < 0 || p.s[p.pos+end] != '\'' {
		return "", p.errf("unterminated string")
	}
	s := p.s[p.pos : p.pos+end]
	p.pos += end + 1
	return s, nil
}

// multiString reads a multi-line string (three quotes, double or single).
// A newline right after the opening quotes is dropped, and in a basic one
// a backslash at a line end joins it to the next non-blank text.
func (p *tomlParser) multiString(q string, basic bool) (string, error) {
	p.pos += 3
	if p.consume("\n") {
		p.line++
	}
	var b strings.Builder
	for {
		if p.eof() {
			return "", p.errf("unterminated string")
		}
		if strings.HasPrefix(p.rest(), q) {
			p.pos += 3
			// Up to two more quotes belong to the string.
			for i := 0; i < 2 && strings.HasPrefix(p.rest(), q[:1]); i++ {
				b.WriteByte(q[0])
				p.pos++
			}
			return b.String(), nil
		}
		c := p.peek()
		switch {
		case c == '\n':
			p.line++
			b.WriteByte(c)
			p.pos++
		case basic && c == '\\':
			r := p.s[p.pos+1:]
			if t := strings.TrimLeft(r, " \t"); strings.HasPrefix(t, "\n") {
				p.pos++
				for !p.eof() && strings.ContainsRune(" \t\n\r", rune(p.peek())) {
					if p.peek() == '\n' {
						p.line++
					}
					p.pos++
				}
				continue
			}
			if err := p.escape(&b); err != nil {
				return "", err
			}
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
}

func (p *tomlParser) array() ([]any, error) {
	p.pos++ // [
	out := []any{}
	for {
		p.skipSpaceAndComments(true)
		if p.consume("]") {
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipSpaceAndComments(true)
		if p.consume(",") {
			continue
		}
		if p.consume("]") {
			return out, nil
		}
		return nil, p.errf("want , or ] in an array")
	}
}

func (p *tomlParser) inlineTable() (map[string]any, error) {
	p.pos++ // {
	out := map[string]any{}
	for {
		p.skipSpaceAndComments(true)
		if p.consume("}") {
			return out, nil
		}
		if err := p.keyValue(out); err != nil {
			return nil, err
		}
		p.skipSpaceAndComments(true)
		if p.consume(",") {
			continue
		}
		if p.consume("}") {
			return out, nil
		}
		return nil, p.errf("want , or } in an inline table")
	}
}
