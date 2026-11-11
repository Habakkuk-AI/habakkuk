package remote

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

func TestRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "habakkuk", FileName)
	reg, err := Load(path)
	if err != nil || len(reg.Remotes) != 0 || reg.Default != "" {
		t.Fatalf("a missing file is an empty registry: %+v, %v", reg, err)
	}
	if err := reg.Add("srv", Remote{Target: "ssh://sam@srv.example.com:2222", Identity: "/k/id", Home: "/var/lib/habakkuk", Bin: "/usr/local/bin/habakkuk"}, false); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add("web", Remote{Target: "https://hk.example.com/", TokenEnv: "HK_TOKEN"}, false); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetDefault("srv"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, reg) {
		t.Fatalf("round trip:\n%+v\nwant\n%+v", got, reg)
	}
	if got.Remotes["web"].Target != "https://hk.example.com" {
		t.Fatalf("target not normalized: %q", got.Remotes["web"].Target)
	}
	if names := got.Names(); !reflect.DeepEqual(names, []string{"srv", "web"}) {
		t.Fatalf("names %q", names)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "token\"") {
		t.Fatalf("no token values in the file: %s", data)
	}

	if err := got.Remove("srv"); err != nil || got.Default != "" {
		t.Fatalf("removing the default clears it: %v, default %q", err, got.Default)
	}
	if err := got.Remove("srv"); err == nil {
		t.Fatal("removing twice should fail")
	}
}

func TestRegistryPermissions(t *testing.T) {
	testutil.UnixOnly(t, "permission bits")
	dir := filepath.Join(t.TempDir(), "habakkuk")
	path := filepath.Join(dir, FileName)
	reg := &Registry{Remotes: map[string]Remote{}}
	for range 2 { // create, then replace
		if err := reg.Save(path); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v, want 0600", st.Mode().Perm())
		}
	}
	st, _ := os.Stat(dir)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestLoadCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a corrupt registry should be an error, not an empty one")
	}
}

func TestRegistryAddRejects(t *testing.T) {
	reg := &Registry{Remotes: map[string]Remote{}}
	for _, c := range []struct {
		name string
		rem  Remote
		want string
	}{
		{"Srv", Remote{Target: "ssh://srv"}, "lower-case"},
		{"local", Remote{Target: "ssh://srv"}, "reserved"},
		{"none", Remote{Target: "ssh://srv"}, "reserved"},
		{"srv", Remote{Target: ""}, "empty target"},
		{"srv", Remote{Target: "ftp://srv"}, "use ssh://"},
		{"srv", Remote{Target: "ssh://srv", TokenEnv: "X"}, "-token-env is for https"},
		{"srv", Remote{Target: "https://srv", Home: "/x"}, "for ssh remotes"},
		{"srv", Remote{Target: "https://srv", TokenEnv: "sk-live-abc123"}, "name of a variable"},
		{"srv", Remote{Target: "ssh://srv", Home: "a\nb"}, "control character"},
		{"srv", Remote{Target: "ssh://srv", Identity: "-oProxyCommand=x"}, "file path"},
	} {
		err := reg.Add(c.name, c.rem, false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Add(%q, %+v) = %v, want %q", c.name, c.rem, err, c.want)
		}
	}
	if err := reg.Add("srv", Remote{Target: "ssh://srv"}, false); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add("srv", Remote{Target: "ssh://other"}, false); err == nil {
		t.Fatal("adding an existing name needs replace")
	}
	if err := reg.Add("srv", Remote{Target: "ssh://other"}, true); err != nil || reg.Remotes["srv"].Target != "ssh://other" {
		t.Fatalf("replace: %v %+v", err, reg.Remotes["srv"])
	}
	if err := reg.SetDefault("nope"); err == nil {
		t.Fatal("an unknown default should fail")
	}
	for _, clear := range []string{"none", "", "local"} {
		reg.Default = "srv"
		if err := reg.SetDefault(clear); err != nil || reg.Default != "" {
			t.Fatalf("SetDefault(%q): %v, default %q", clear, err, reg.Default)
		}
	}
}

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]string{
		"ssh://sam@srv.example.com":      "ssh://sam@srv.example.com",
		"ssh://sam@srv.example.com:2222": "ssh://sam@srv.example.com:2222",
		"sam@srv":                        "ssh://sam@srv",
		"srv":                           "ssh://srv",
		"ssh://sam@[::1]:22":             "ssh://sam@[::1]:22",
		"ssh://[fe80::1]":               "ssh://[fe80::1]",
		"https://hk.example.com/":       "https://hk.example.com",
		"https://hk.example.com/base/":  "https://hk.example.com/base",
		"http://127.0.0.1:8484":         "http://127.0.0.1:8484",
		"http://localhost:8484":         "http://localhost:8484",
		"http://[::1]:8484":             "http://[::1]:8484",
	} {
		tg, err := ParseTarget(in)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", in, err)
			continue
		}
		if got := tg.String(); got != want {
			t.Errorf("ParseTarget(%q) = %s, want %s", in, got, want)
		}
		if again, err := ParseTarget(tg.String()); err != nil || again.String() != want {
			t.Errorf("%q does not parse back: %v", want, err)
		}
	}
	tg, _ := ParseTarget("ssh://sam@[::1]:2200")
	if tg.Kind != SSH || tg.Dest() != "sam@::1" || tg.Port != 2200 {
		t.Fatalf("ipv6: %+v dest %q", tg, tg.Dest())
	}
	for _, bad := range []string{
		"http://hk.example.com",          // a token in the clear
		"https://user:pw@hk.example.com", // credentials in the URL
		"https://hk.example.com/?token=x",
		"ssh://sam:pw@srv",
		"ssh://-oProxyCommand=x",
		"ssh://-x@srv",
		"ssh://srv:0",
		"ssh://srv:99999",
		"ssh://srv/path",
		"ssh://a;b@srv",
		"https://",
	} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) should fail", bad)
		}
	}
}

func TestSelectPrecedence(t *testing.T) {
	reg := &Registry{Default: "def", Remotes: map[string]Remote{
		"def": {Target: "ssh://def"}, "env": {Target: "ssh://env"}, "flag": {Target: "ssh://flag"},
	}}
	for _, c := range []struct {
		flag, env, def string
		want           string // "" means local
		err            bool
	}{
		{"", "", "", "", false},
		{"", "", "def", "def", false},
		{"", "env", "def", "env", false},
		{"flag", "env", "def", "flag", false},
		{"local", "env", "def", "", false},
		{"", "local", "def", "", false},
		{"nope", "", "", "", true},
		{"", "nope", "def", "", true},
		{"", "", "gone", "", true},
	} {
		reg.Default = c.def
		name, rem, ok, err := Select(c.flag, c.env, reg)
		if (err != nil) != c.err {
			t.Errorf("%+v: err %v", c, err)
			continue
		}
		if name != c.want || ok != (c.want != "") || (ok && rem.Target != "ssh://"+c.want) {
			t.Errorf("%+v: got %q %v %+v", c, name, ok, rem)
		}
	}
	if _, _, ok, err := Select("", "", nil); ok || err != nil {
		t.Fatal("no registry is local")
	}
}

func TestRisky(t *testing.T) {
	for _, c := range []struct {
		args  string
		risky bool
	}{
		{"serve", true},
		{"init", true},
		{"restore backup.tar.gz", true},
		{"update", true},
		{"update -channel beta", true},
		{"update --rollback", true},
		{"update -check", false},
		{"update --check", false},
		{"migrate", true},
		{"migrate --from x --dry-run", false},
		{"setup service", true},
		{"setup", false},
		{"setup model", false},
		{"config migrate", true},
		{"config migrate -dry-run", false},
		{"status", false},
		{"chat", false},
		{"run serve", false},
		{"", false},
	} {
		if got := Risky(strings.Fields(c.args)) != ""; got != c.risky {
			t.Errorf("Risky(%q) = %v, want %v", c.args, got, c.risky)
		}
	}
}
