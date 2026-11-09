package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// decodeJSON5 reads the JSON5 used by hand-edited config files: comments,
// unquoted keys, single-quoted strings, trailing commas, hexadecimal
// numbers, +Infinity and NaN aside. It converts the text to JSON and
// decodes that.
func decodeJSON5(data []byte, v any) error {
	js, err := json5ToJSON(data)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.UseNumber()
	return dec.Decode(v)
}

func json5ToJSON(src []byte) ([]byte, error) {
	s := []rune(string(src))
	var out strings.Builder
	line := 1
	fail := func(format string, args ...any) error {
		return fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, args...))
	}
	// pendingComma holds a comma until the next token shows it is not a
	// trailing one.
	pendingComma := false
	flushComma := func() {
		if pendingComma {
			out.WriteByte(',')
			pendingComma = false
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\n':
			line++
			out.WriteRune(c)
			i++
		case unicode.IsSpace(c) || c == '\uFEFF':
			i++
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(string(s[i+2:]), "*/")
			if end < 0 {
				return nil, fail("unclosed comment")
			}
			body := string(s[i+2:])[:end]
			line += strings.Count(body, "\n")
			i += 2 + len([]rune(body)) + 2
		case c == ',':
			pendingComma = true
			i++
		case c == '}' || c == ']':
			pendingComma = false // a trailing comma
			out.WriteRune(c)
			i++
		case c == '"' || c == '\'':
			flushComma()
			str, n, err := readString(s[i:])
			if err != nil {
				return nil, fail("%v", err)
			}
			line += strings.Count(str, "\n")
			b, _ := json.Marshal(str)
			out.Write(b)
			i += n
		case c == '{' || c == '[' || c == ':':
			flushComma()
			out.WriteRune(c)
			i++
		default:
			flushComma()
			j := i
			for j < len(s) && !strings.ContainsRune(",:{}[]\"'/", s[j]) && !unicode.IsSpace(s[j]) {
				j++
			}
			if j == i {
				return nil, fail("unexpected %q", c)
			}
			word := string(s[i:j])
			// A bare word before a colon is a key.
			k := j
			for k < len(s) && unicode.IsSpace(s[k]) && s[k] != '\n' {
				k++
			}
			if k < len(s) && s[k] == ':' {
				b, _ := json.Marshal(word)
				out.Write(b)
				i = j
				continue
			}
			lit, err := literal(word)
			if err != nil {
				return nil, fail("%v", err)
			}
			out.WriteString(lit)
			i = j
		}
	}
	return []byte(out.String()), nil
}

// readString reads a quoted JSON5 string starting at s[0] and returns its
// value and the runes consumed.
func readString(s []rune) (string, int, error) {
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == q:
			return b.String(), i + 1, nil
		case c == '\\' && i+1 < len(s):
			i++
			switch e := s[i]; e {
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
			case '0':
				b.WriteByte(0)
			case '\n':
				// a line continuation
			case 'u':
				if i+4 >= len(s) {
					return "", 0, fmt.Errorf("bad \\u escape")
				}
				n, err := strconv.ParseUint(string(s[i+1:i+5]), 16, 32)
				if err != nil {
					return "", 0, fmt.Errorf("bad \\u escape")
				}
				b.WriteRune(rune(n))
				i += 4
			case 'x':
				if i+2 >= len(s) {
					return "", 0, fmt.Errorf("bad \\x escape")
				}
				n, err := strconv.ParseUint(string(s[i+1:i+3]), 16, 8)
				if err != nil {
					return "", 0, fmt.Errorf("bad \\x escape")
				}
				b.WriteRune(rune(n))
				i += 2
			default:
				b.WriteRune(e)
			}
		default:
			b.WriteRune(c)
		}
	}
	return "", 0, fmt.Errorf("unclosed string")
}

// literal turns a bare JSON5 value into JSON.
func literal(w string) (string, error) {
	switch w {
	case "true", "false", "null":
		return w, nil
	case "Infinity", "+Infinity", "-Infinity", "NaN", "+NaN", "-NaN":
		return "null", nil
	}
	sign := ""
	if strings.HasPrefix(w, "+") || strings.HasPrefix(w, "-") {
		if w[0] == '-' {
			sign = "-"
		}
		w = w[1:]
	}
	if h, ok := strings.CutPrefix(strings.ToLower(w), "0x"); ok {
		n, err := strconv.ParseInt(h, 16, 64)
		if err != nil {
			return "", fmt.Errorf("bad number %q", w)
		}
		return sign + strconv.FormatInt(n, 10), nil
	}
	if strings.HasPrefix(w, ".") {
		w = "0" + w
	}
	w = strings.TrimSuffix(w, ".")
	if _, err := strconv.ParseFloat(w, 64); err != nil {
		return "", fmt.Errorf("unexpected %q", w)
	}
	return sign + w, nil
}
