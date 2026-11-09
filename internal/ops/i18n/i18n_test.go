package i18n

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestCatalogsHaveTheSameKeys(t *testing.T) {
	cats, err := Catalogs()
	if err != nil {
		t.Fatal(err)
	}
	en := cats[Default]
	if len(en) < 100 {
		t.Fatalf("English catalog has %d keys; expected the full set", len(en))
	}
	for _, code := range Supported() {
		cat, ok := cats[code]
		if !ok {
			t.Errorf("no catalog for supported language %s", code)
			continue
		}
		for k := range en {
			if v, ok := cat[k]; !ok || strings.TrimSpace(v) == "" {
				t.Errorf("%s: missing or empty key %s", code, k)
			}
		}
		for k := range cat {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: key %s is not in en.yaml", code, k)
			}
		}
	}
	for code := range cats {
		if _, ok := names[code]; !ok {
			t.Errorf("catalog %s.yaml has no entry in names", code)
		}
	}
}

var verb = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?([a-zA-Z%])`)

// verbs lists a format's verbs, sorted, ignoring explicit argument indexes,
// so a translation that reorders its arguments still matches.
func verbs(s string) []string {
	var out []string
	for _, m := range verb.FindAllStringSubmatch(s, -1) {
		if m[3] != "%" {
			out = append(out, m[3])
		}
	}
	slices.Sort(out)
	return out
}

func TestTranslationsKeepTheirVerbs(t *testing.T) {
	cats, _ := Catalogs()
	for code, cat := range cats {
		for k, v := range cat {
			if want, got := verbs(cats[Default][k]), verbs(v); !slices.Equal(want, got) {
				t.Errorf("%s %s: verbs %v, English has %v", code, k, got, want)
			}
		}
	}
}

func TestMachineTranslatedCatalogsSaySo(t *testing.T) {
	for _, code := range Supported() {
		if code == Default {
			continue
		}
		data, err := files.ReadFile("locales/" + code + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		if head, _, _ := strings.Cut(string(data), "\n\n"); !strings.Contains(head, "Machine-translated") {
			t.Errorf("%s.yaml does not say it is machine-translated in its header", code)
		}
	}
}

func TestLookupAndFallback(t *testing.T) {
	if got := T("en", "new.done"); got != "Started a fresh conversation." {
		t.Errorf("en: %q", got)
	}
	if got := T("fr", "new.done"); got != "Nouvelle conversation commencée." {
		t.Errorf("fr: %q", got)
	}
	if got := T("pt-BR", "title.current", "X"); got != "Título: X" {
		t.Errorf("pt-BR: %q", got)
	}
	// Unknown languages and empty settings fall back to English.
	if got := T("xx", "new.done"); got != "Started a fresh conversation." {
		t.Errorf("unknown language: %q", got)
	}
	if got := T("", "turn.done"); got != "Done." {
		t.Errorf("empty language: %q", got)
	}
	// A key English lacks shows as itself.
	if got := T("fr", "no.such.key"); got != "no.such.key" {
		t.Errorf("missing key: %q", got)
	}
	if Has("no.such.key") || !Has("turn.done") {
		t.Error("Has is wrong")
	}
}

func TestCountsAndReordering(t *testing.T) {
	if got := N("en", "stop.done", 1); got != "Stopped (1 task)." {
		t.Errorf("one: %q", got)
	}
	if got := N("en", "stop.done", 3); got != "Stopped (3 tasks)." {
		t.Errorf("other: %q", got)
	}
	got := N("en", "rewind.done", 1, "s2", "s1")
	if want := "Rewound: we carry on from before your last message (session s2). The original conversation is kept as s1."; got != want {
		t.Errorf("rewind one:\n got %q\nwant %q", got, want)
	}
	if got := N("de", "rewind.done", 4, "s2", "s1"); !strings.Contains(got, "letzten 4 Nachrichten") || strings.Contains(got, "%!") {
		t.Errorf("rewind other: %q", got)
	}
	// Japanese reorders goal.current's counts.
	if got := T("ja", "goal.current", "G", 2, 10); !strings.Contains(got, "最大 10 ターン中 2 ターン") {
		t.Errorf("ja reorder: %q", got)
	}
}

func TestNormalizeAndPick(t *testing.T) {
	for in, want := range map[string]string{
		"fr": "fr", "FR": "fr", "fr_CA.UTF-8": "fr", "pt-BR": "pt", "zh-Hans-CN": "zh", "de_DE@euro": "de",
		"yo-NG": "yo", "klingon": "", "": "", "C": "",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Pick("", "es"); got != "es" {
		t.Errorf("Pick global: %q", got)
	}
	if got := Pick("ja", "es"); got != "ja" {
		t.Errorf("Pick channel override: %q", got)
	}
	if got := Pick("xx", ""); got != Default {
		t.Errorf("Pick fallback: %q", got)
	}
	if !Known("") || !Known("hi-IN") || Known("tlh") {
		t.Error("Known is wrong")
	}
	if Name("zh") != "Chinese (Simplified)" || Name("tlh") != "tlh" {
		t.Error("Name is wrong")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "es_MX.UTF-8")
	if got := FromEnv(); got != "es" {
		t.Errorf("FromEnv = %q", got)
	}
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "C.UTF-8")
	if got := FromEnv(); got != Default {
		t.Errorf("FromEnv with C = %q", got)
	}
}

func TestWordsIncludeEnglish(t *testing.T) {
	yes := Words("fr", "approval.yes_words")
	for _, w := range []string{"oui", "d'accord", "yes", "ok"} {
		if !slices.Contains(yes, w) {
			t.Errorf("French yes words lack %q: %v", w, yes)
		}
	}
	if slices.Contains(yes, "non") {
		t.Error("yes words contain a no word")
	}
	for _, code := range Supported() {
		y, n := Words(code, "approval.yes_words"), Words(code, "approval.no_words")
		for _, w := range y {
			if slices.Contains(n, w) {
				t.Errorf("%s: %q is both a yes and a no word", code, w)
			}
		}
	}
}

func TestPrinter(t *testing.T) {
	p := Printer("es")
	if got := p.T("status.agent", "habakkuk"); got != "Agente: habakkuk" {
		t.Errorf("Printer.T: %q", got)
	}
	if got := p.N("stop.done", 2); got != "Detenido (2 tareas)." {
		t.Errorf("Printer.N: %q", got)
	}
}
