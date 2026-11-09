// Package i18n holds the text Habakkuk shows people itself, outside what a
// model writes: chat command replies, approval prompts, errors shown in a
// chat and some CLI messages. Each locale is a flat YAML catalog embedded in
// the binary (locales/<code>.yaml); a key missing from a locale falls back
// to English, and a key missing from English is shown as the key, so a gap
// is visible rather than blank.
//
// Values are fmt formats. A translation may reorder its arguments with
// explicit indexes (%[2]s), but must use the same verbs as English.
package i18n

import (
	"embed"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Default is the language used when none is configured or the configured
// one has no catalog.
const Default = "en"

//go:embed locales/*.yaml
var files embed.FS

// names are the languages Habakkuk ships, by code, with their English names
// (for the model's system prompt and error messages).
var names = map[string]string{
	"en": "English",
	"fr": "French",
	"es": "Spanish",
	"pt": "Portuguese",
	"de": "German",
	"yo": "Yoruba",
	"ar": "Arabic",
	"zh": "Chinese (Simplified)",
	"ja": "Japanese",
	"hi": "Hindi",
}

var (
	loadOnce sync.Once
	catalogs map[string]map[string]string
	loadErr  error
)

func load() {
	catalogs = map[string]map[string]string{}
	entries, err := files.ReadDir("locales")
	if err != nil {
		loadErr = err
		return
	}
	for _, e := range entries {
		code := strings.TrimSuffix(e.Name(), path.Ext(e.Name()))
		data, err := files.ReadFile("locales/" + e.Name())
		if err != nil {
			loadErr = err
			return
		}
		cat := map[string]string{}
		if err := yaml.Unmarshal(data, &cat); err != nil {
			loadErr = fmt.Errorf("i18n: %s: %w", e.Name(), err)
			return
		}
		catalogs[code] = cat
	}
}

// Catalogs returns every embedded catalog by language code. It is for tests
// and tooling; callers must not change the maps.
func Catalogs() (map[string]map[string]string, error) {
	loadOnce.Do(load)
	return catalogs, loadErr
}

// Supported lists the language codes Habakkuk has catalogs for, sorted.
func Supported() []string {
	out := make([]string, 0, len(names))
	for code := range names {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// Name is a language's English name ("French"), or the code itself when it
// is not one Habakkuk ships.
func Name(lang string) string {
	if n, ok := names[Normalize(lang)]; ok {
		return n
	}
	return lang
}

// Normalize maps a language tag or locale ("pt-BR", "fr_CA.UTF-8",
// "zh-Hans-CN", "ZH") to the code of the catalog that serves it, or "" when
// none does.
func Normalize(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(tag, ".@"); i >= 0 { // fr_FR.UTF-8, de_DE@euro
		tag = tag[:i]
	}
	base, _, _ := strings.Cut(strings.ReplaceAll(tag, "_", "-"), "-")
	if _, ok := names[base]; ok {
		return base
	}
	return ""
}

// Known reports whether a configured language names a catalog Habakkuk has
// (after Normalize). The empty string is known: it means the default.
func Known(lang string) bool { return lang == "" || Normalize(lang) != "" }

// Pick returns the first of the given languages Habakkuk has a catalog for,
// in the order given (a channel's override before the global setting), or
// Default.
func Pick(langs ...string) string {
	for _, l := range langs {
		if code := Normalize(l); code != "" {
			return code
		}
	}
	return Default
}

// FromEnv is the language of the user's locale environment (LC_ALL,
// LC_MESSAGES, LANG), for CLI messages when the config names none.
func FromEnv() string {
	for _, v := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if code := Normalize(os.Getenv(v)); code != "" {
			return code
		}
	}
	return Default
}

// Lookup returns the raw text for key in lang, falling back to English and
// then to the key itself.
func Lookup(lang, key string) string {
	loadOnce.Do(load)
	if cat := catalogs[Normalize(lang)]; cat != nil {
		if v, ok := cat[key]; ok && v != "" {
			return v
		}
	}
	if v, ok := catalogs[Default][key]; ok {
		return v
	}
	return key
}

// Has reports whether English defines key, so callers can fall back to
// their own text for keys a catalog does not know.
func Has(key string) bool {
	loadOnce.Do(load)
	_, ok := catalogs[Default][key]
	return ok
}

// T formats the text for key in lang with args.
func T(lang, key string, args ...any) string {
	text := Lookup(lang, key)
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// N is T for a count: it uses key+".one" when n is 1 and key+".other"
// otherwise. n is the first argument; a format leaves it out by naming the
// rest with explicit indexes.
func N(lang, key string, n int, args ...any) string {
	k := key + ".other"
	if n == 1 {
		k = key + ".one"
	}
	return T(lang, k, append([]any{n}, args...)...)
}

// Words splits a comma-separated list entry (approval.yes_words) into
// lower-cased words, adding English's so an English reply always works.
func Words(lang, key string) []string {
	var out []string
	add := func(list string) {
		for w := range strings.SplitSeq(list, ",") {
			if w = strings.ToLower(strings.TrimSpace(w)); w != "" && !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	add(Lookup(lang, key))
	add(Lookup(Default, key))
	return out
}

// Printer formats messages in one language.
type Printer string

// T formats key's text in the printer's language.
func (p Printer) T(key string, args ...any) string { return T(string(p), key, args...) }

// N formats a counted message in the printer's language.
func (p Printer) N(key string, n int, args ...any) string { return N(string(p), key, n, args...) }
