package migrate

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
)

// Imported text often names paths inside the source home: a schedule that
// runs ~/.hermes/scripts/sync.py, a skill that reads
// /home/ada/.hermes/workspaces/briefs. homeRefs finds those references and
// a rewriter points them somewhere else.
//
// Everything here works on strings, never on the file system, and takes
// the path style (Unix or Windows) as a setting, so both styles are tested
// on any computer.

// The ways text spells the source home.
const (
	formAbs     = iota // /home/ada/.hermes, C:\Users\ada\.hermes
	formTilde          // ~/.hermes
	formVar            // $HOME/.hermes
	formBrace          // ${HOME}/.hermes
	formPercent        // %USERPROFILE%\.hermes (Windows)
	formPS             // $env:USERPROFILE\.hermes (Windows)
)

// formToken is how each form spells the user's home.
var formToken = map[int]string{formTilde: "~", formVar: "$HOME", formBrace: "${HOME}",
	formPercent: "%USERPROFILE%", formPS: "$env:USERPROFILE"}

// homeRefs recognises references into one source home.
type homeRefs struct {
	windows  bool
	home     string          // the source home, absolute
	userHome string          // the user's home directory, absolute; "" when unknown
	nested   map[string]bool // homes inside the home, as "profiles/coder"
	// odd are the entries whose names hold characters people usually quote
	// (a space, a bracket), by the home they are in ("" or a nested one),
	// longest first. Such a name is only recognised when it is known.
	odd    map[string][]string
	re     *regexp.Regexp
	groups []int  // the form of each capture group of re
	base   string // the home's last path element, for a quick first check
}

// ref is one reference: text[start:end] spells the home and the entry.
type ref struct {
	start, end int
	form       int
	sep        string // the separator the text used after the home
	key        string // the entry under the home ("scripts", "profiles/coder/state"); "" is the home itself
}

// newHomeRefs prepares to find references into home. nested lists homes
// kept inside it (slash paths relative to it); an entry of a nested home
// is named by its whole relative path.
func newHomeRefs(home, userHome string, nested []string, windows bool) *homeRefs {
	h := &homeRefs{windows: windows, home: home, userHome: userHome, nested: map[string]bool{}}
	for _, n := range nested {
		h.nested[h.fold(n)] = true
	}
	vol, parts := splitPath(home, windows)
	if len(parts) > 0 {
		h.base = parts[len(parts)-1]
	}
	sep := "/"
	if windows {
		sep = `[\\/]+`
	}
	join := func(prefix string, parts []string) string {
		var b strings.Builder
		b.WriteString(prefix)
		for _, p := range parts {
			b.WriteString(sep)
			b.WriteString(regexp.QuoteMeta(p))
		}
		return b.String()
	}
	var alts []string
	add := func(form int, pattern string) {
		alts = append(alts, "("+pattern+")")
		h.groups = append(h.groups, form)
	}
	if share := shareParts(vol); len(share) > 0 { // a network share
		add(formAbs, `[\\/]{2,}`+regexp.QuoteMeta(share[0])+join("", append(share[1:], parts...)))
	} else {
		add(formAbs, join(regexp.QuoteMeta(vol), parts))
	}
	if rel, ok := relPath(userHome, home, windows); ok && len(rel) > 0 {
		add(formTilde, join("~", rel))
		add(formVar, join(`\$HOME`, rel))
		add(formBrace, join(`\$\{HOME\}`, rel))
		if windows {
			add(formPercent, join(`%USERPROFILE%`, rel))
			add(formPS, join(`\$env:USERPROFILE`, rel))
		}
	}
	flags := ""
	if windows {
		flags = "(?i)"
	}
	h.re = regexp.MustCompile(flags + strings.Join(alts, "|"))
	if len(parts) == 0 {
		h.re = regexp.MustCompile(`[^\s\S]`) // the home is a file system root: nothing is a reference
	}
	return h
}

// know tells h the entries a home really holds (parent is "" or a nested
// home), so that a name with a space in it is read whole.
func (h *homeRefs) know(parent string, names []string) {
	var odd []string
	for _, n := range names {
		if strings.IndexFunc(n, func(r rune) bool { return r < 0x80 && !nameByte(byte(r)) }) >= 0 {
			odd = append(odd, n)
		}
	}
	slices.SortFunc(odd, func(a, b string) int { return len(b) - len(a) })
	if h.odd == nil {
		h.odd = map[string][]string{}
	}
	h.odd[h.fold(parent)] = odd
}

// oddName returns the known entry of parent that text[k:] starts with, if
// its name is one nameByte would cut short.
func (h *homeRefs) oddName(parent, text string, k int) (string, bool) {
	for _, n := range h.odd[h.fold(parent)] {
		end := k + len(n)
		if end > len(text) || h.fold(text[k:end]) != h.fold(n) {
			continue
		}
		if end == len(text) || h.isSep(text[end]) || !nameByte(text[end]) || h.fullStop(text, end) {
			return n, true
		}
	}
	return "", false
}

func (h *homeRefs) fold(s string) string {
	if h.windows {
		return strings.ToLower(s)
	}
	return s
}

func (h *homeRefs) isSep(c byte) bool { return c == '/' || h.windows && c == '\\' }

// nameByte reports whether c can be part of a file name the way people
// write them without quotes.
func nameByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c >= 0x80:
		return true
	}
	return strings.IndexByte("._-+@~", c) >= 0
}

// scan returns the references in text, in order.
func (h *homeRefs) scan(text string) []ref {
	if !h.windows && h.base != "" && !strings.Contains(text, h.base) {
		return nil
	}
	var out []ref
	for _, m := range h.re.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[0], m[1]
		form := formAbs
		for g, f := range h.groups {
			if m[2+2*g] >= 0 {
				form = f
			}
		}
		// The home must start a path: /mnt/home/ada/.hermes is another folder.
		if start > 0 {
			if c := text[start-1]; nameByte(c) || c == '/' || c == '\\' || c == '$' || c == '}' {
				continue
			}
		}
		// And end one: ~/.hermes-old and ~/.hermes.bak are other folders.
		if end < len(text) && !h.isSep(text[end]) && nameByte(text[end]) && !h.fullStop(text, end) {
			continue
		}
		r := ref{start: start, end: end, form: form, sep: "/"}
		if h.windows {
			r.sep = `\`
		}
		// Up to three path elements after the home: one names the entry,
		// three name an entry of a nested home.
		type element struct {
			name string
			end  int
		}
		var els []element
		for at := end; len(els) < 3; {
			k := at
			for k < len(text) && h.isSep(text[k]) {
				k++
			}
			if k == at {
				break
			}
			n := k
			for n < len(text) && nameByte(text[n]) {
				n++
			}
			// A full stop that ends the sentence is not part of the name.
			for n > k && text[n-1] == '.' && h.fullStop(text, n-1) {
				n--
			}
			name := text[k:n]
			// An entry (never a path inside one) may have a name that
			// stops early here: "My Notes".
			if parent := ""; len(els) == 0 || len(els) == 2 && h.nested[h.fold(els[0].name+"/"+els[1].name)] {
				if len(els) == 2 {
					parent = els[0].name + "/" + els[1].name
				}
				if odd, ok := h.oddName(parent, text, k); ok {
					name, n = odd, k+len(odd)
				}
			}
			if name == "" || name == "." || name == ".." {
				break
			}
			if len(els) == 0 {
				r.sep = text[at:k]
			}
			els = append(els, element{name, n})
			at = n
		}
		switch {
		case len(els) == 3 && h.nested[h.fold(els[0].name+"/"+els[1].name)]:
			r.key, r.end = h.fold(els[0].name+"/"+els[1].name+"/"+els[2].name), els[2].end
		case len(els) > 0:
			r.key, r.end = h.fold(els[0].name), els[0].end
		}
		out = append(out, r)
	}
	return out
}

// fullStop reports whether the "." at text[i] ends a sentence rather than
// starting a file extension.
func (h *homeRefs) fullStop(text string, i int) bool {
	if text[i] != '.' {
		return false
	}
	return i+1 == len(text) || !nameByte(text[i+1]) && !h.isSep(text[i+1])
}

// count returns how many times text names each entry.
func (h *homeRefs) count(text string) map[string]int {
	var out map[string]int
	for _, r := range h.scan(text) {
		if out == nil {
			out = map[string]int{}
		}
		out[r.key]++
	}
	return out
}

// spell writes dest the way a reference of the given form and separator
// would: under the user's home with the same "~" or "$HOME" the text used,
// otherwise as an absolute path.
func (h *homeRefs) spell(dest string, form int, sep string) string {
	if form != formAbs {
		if rel, ok := relPath(h.userHome, dest, h.windows); ok && len(rel) > 0 {
			return formToken[form] + sep + strings.Join(rel, sep)
		}
	}
	if !h.windows {
		return dest
	}
	vol, parts := splitPath(dest, true)
	if share := shareParts(vol); len(share) > 0 {
		return sep + sep + strings.Join(append(share, parts...), sep)
	}
	return vol + sep + strings.Join(parts, sep)
}

// shareParts returns the server and share of a \\server\share volume, or
// nil for any other volume.
func shareParts(vol string) []string {
	if !strings.HasPrefix(vol, `\\`) {
		return nil
	}
	return strings.FieldsFunc(vol, func(r rune) bool { return r == '\\' || r == '/' })
}

// rewriter points references to entries at their new places.
type rewriter struct {
	h  *homeRefs
	to map[string]string // entry -> its new absolute path
}

// moves reports whether references to the entry are rewritten.
func (r *rewriter) moves(key string) bool {
	if r == nil {
		return false
	}
	_, ok := r.to[key]
	return ok
}

// movesAny reports whether any of the entries named in counts moves.
func (r *rewriter) movesAny(counts map[string]int) bool {
	for key := range counts {
		if r.moves(key) {
			return true
		}
	}
	return false
}

// apply rewrites the references in text and reports how many it changed.
// A nil rewriter changes nothing.
func (r *rewriter) apply(text string) (string, int) {
	if r == nil || len(r.to) == 0 || text == "" {
		return text, 0
	}
	var b strings.Builder
	last, n := 0, 0
	for _, f := range r.h.scan(text) {
		dest, ok := r.to[f.key]
		if !ok {
			continue
		}
		b.WriteString(text[last:f.start])
		b.WriteString(r.h.spell(dest, f.form, f.sep))
		last = f.end
		n++
	}
	if n == 0 {
		return text, 0
	}
	b.WriteString(text[last:])
	return b.String(), n
}

// str is apply without the count.
func (r *rewriter) str(text string) string {
	out, _ := r.apply(text)
	return out
}

// strs is str for each of a list.
func (r *rewriter) strs(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = r.str(s)
	}
	return out
}

// bytes rewrites the references in a file's contents; binary contents are
// returned as they are.
func (r *rewriter) bytes(data []byte) ([]byte, int) {
	if r == nil || len(r.to) == 0 || !isText(data) {
		return data, 0
	}
	out, n := r.apply(string(data))
	if n == 0 {
		return data, 0
	}
	return []byte(out), n
}

// isText reports whether a file's contents look like text.
func isText(data []byte) bool {
	return len(data) > 0 && bytes.IndexByte(data, 0) < 0
}

// splitPath splits an absolute path into its volume ("" on Unix, "C:" or
// \\server\share on Windows) and its elements.
func splitPath(p string, windows bool) (vol string, parts []string) {
	isSep := func(r rune) bool { return r == '/' || windows && r == '\\' }
	if windows {
		switch {
		case len(p) >= 2 && p[1] == ':':
			vol, p = p[:2], p[2:]
		case len(p) >= 2 && isSep(rune(p[0])) && isSep(rune(p[1])):
			f := strings.FieldsFunc(p, isSep)
			if len(f) >= 2 {
				return `\\` + f[0] + `\` + f[1], f[2:]
			}
			return `\\` + strings.Join(f, `\`), nil
		}
	}
	return vol, strings.FieldsFunc(p, isSep)
}

// relPath returns the elements of p below base, and whether p is base or
// below it.
func relPath(base, p string, windows bool) ([]string, bool) {
	if base == "" || p == "" {
		return nil, false
	}
	eq := func(a, b string) bool {
		if windows {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	bv, bp := splitPath(base, windows)
	pv, pp := splitPath(p, windows)
	if !eq(bv, pv) || len(pp) < len(bp) {
		return nil, false
	}
	for i := range bp {
		if !eq(bp[i], pp[i]) {
			return nil, false
		}
	}
	return pp[len(bp):], true
}
