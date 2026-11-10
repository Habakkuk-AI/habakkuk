package migrate

import (
	"maps"
	"testing"
)

func TestHomeRefsFindsEveryForm(t *testing.T) {
	h := newHomeRefs("/home/ada/.hermes", "/home/ada", []string{"profiles/coder"}, false)
	cases := []struct {
		text string
		want map[string]int // entry -> references
	}{
		{"python3 /home/ada/.hermes/scripts/sync.py", map[string]int{"scripts": 1}},
		{"briefs are in ~/.hermes/workspaces/briefs and ~/.hermes/workspaces/reports", map[string]int{"workspaces": 2}},
		{`. "$HOME/.hermes/.env"`, map[string]int{".env": 1}},
		{`cat "${HOME}/.hermes/state/x.json" > ${HOME}/.hermes/state/y.json`, map[string]int{"state": 2}},
		{"PATH=/usr/bin:/home/ada/.hermes/bin", map[string]int{"bin": 1}},
		{"STATE='/home/ada/.hermes/state/inbox.json'", map[string]int{"state": 1}},
		{"see ~/.hermes/notes.txt.", map[string]int{"notes.txt": 1}},
		{"Look in ~/.hermes/workspaces.", map[string]int{"workspaces": 1}},
		// A profile's home: its entries are named with the profile.
		{"run ~/.hermes/profiles/coder/scripts/lint.sh", map[string]int{"profiles/coder/scripts": 1}},
		{"ls ~/.hermes/profiles/coder", map[string]int{"profiles": 1}},
		{"ls ~/.hermes/profiles/other/scripts", map[string]int{"profiles": 1}},
		// The folder as a whole.
		{"cd ~/.hermes && ls", map[string]int{"": 1}},
		{"everything under /home/ada/.hermes.", map[string]int{"": 1}},
		{"tar czf backup.tgz ~/.hermes/", map[string]int{"": 1}},
		{"find ~/.hermes/*.json", map[string]int{"": 1}},
		// Other folders that only look alike.
		{"~/.hermes-old/scripts and ~/.hermes.bak/x and ~/.hermes2/x", nil},
		{"/mnt/backup/home/ada/.hermes/scripts", nil},
		{"/home/adam/.hermes/scripts and /home/ada/.hermesx", nil},
		{"$XDG/home/ada/.hermes/scripts", nil},
		{"nothing to see here", nil},
	}
	for _, c := range cases {
		if got := h.count(c.text); !maps.Equal(got, c.want) {
			t.Errorf("%q: found %v, want %v", c.text, got, c.want)
		}
	}
}

func TestRewriterKeepsTheFormTheTextUsed(t *testing.T) {
	h := newHomeRefs("/home/ada/.hermes", "/home/ada", nil, false)
	rw := &rewriter{h: h, to: map[string]string{
		"scripts":    "/home/ada/.habakkuk/files/scripts",
		"workspaces": "/srv/data/workspaces", // outside the user's home
	}}
	cases := []struct{ in, want string }{
		{"python3 /home/ada/.hermes/scripts/sync.py", "python3 /home/ada/.habakkuk/files/scripts/sync.py"},
		{"python3 ~/.hermes/scripts/sync.py", "python3 ~/.habakkuk/files/scripts/sync.py"},
		{`sh "$HOME/.hermes/scripts/a.sh"`, `sh "$HOME/.habakkuk/files/scripts/a.sh"`},
		{`sh "${HOME}/.hermes/scripts/a.sh"`, `sh "${HOME}/.habakkuk/files/scripts/a.sh"`},
		// No "~" when the new place is not under the user's home.
		{"ls ~/.hermes/workspaces/briefs", "ls /srv/data/workspaces/briefs"},
		{"ls $HOME/.hermes/workspaces", "ls /srv/data/workspaces"},
		// What does not move is left exactly as it was.
		{". ~/.hermes/.env; ls ~/.hermes; ls ~/.hermes/scripts.", ". ~/.hermes/.env; ls ~/.hermes; ls ~/.habakkuk/files/scripts."},
		{"~/.hermes/scriptsx/a", "~/.hermes/scriptsx/a"},
	}
	for _, c := range cases {
		got, _ := rw.apply(c.in)
		if got != c.want {
			t.Errorf("%q\n  became %q\n  want   %q", c.in, got, c.want)
		}
		// Rewriting what is already rewritten changes nothing.
		if again, n := rw.apply(got); again != got || n != 0 {
			t.Errorf("%q rewritten twice became %q (%d more)", c.in, again, n)
		}
	}
	if _, n := rw.apply("~/.hermes/scripts/a ~/.hermes/scripts/b ~/.hermes/state/c"); n != 2 {
		t.Errorf("counted %d rewritten references, want 2", n)
	}
	var none *rewriter
	if got, n := none.apply("~/.hermes/scripts/a"); got != "~/.hermes/scripts/a" || n != 0 {
		t.Errorf("a nil rewriter changed the text to %q", got)
	}
	if out, n := rw.bytes([]byte("bin\x00~/.hermes/scripts/a")); n != 0 || string(out) != "bin\x00~/.hermes/scripts/a" {
		t.Error("a binary file was rewritten")
	}
}

// Windows paths are recognised on any computer: the path style is a
// setting, not the operating system the tests run on.
func TestHomeRefsWindowsPaths(t *testing.T) {
	h := newHomeRefs(`C:\Users\ada\.hermes`, `C:\Users\ada`, []string{"profiles/coder"}, true)
	found := []struct {
		text string
		want map[string]int
	}{
		{`python C:\Users\ada\.hermes\scripts\sync.py`, map[string]int{"scripts": 1}},
		{`python C:/Users/ada/.hermes/scripts/sync.py`, map[string]int{"scripts": 1}},
		{`{"state": "C:\\Users\\ada\\.hermes\\state\\inbox.json"}`, map[string]int{"state": 1}},
		{`type c:\users\ADA\.Hermes\State\inbox.json`, map[string]int{"state": 1}}, // case does not matter there
		{`dir %USERPROFILE%\.hermes\workspaces`, map[string]int{"workspaces": 1}},
		{`ls $env:USERPROFILE\.hermes\workspaces\briefs`, map[string]int{"workspaces": 1}},
		{`ls ~\.hermes\workspaces and ~/.hermes/scripts`, map[string]int{"workspaces": 1, "scripts": 1}},
		{`run C:\Users\ada\.hermes\profiles\coder\scripts\lint.cmd`, map[string]int{"profiles/coder/scripts": 1}},
		{`cd C:\Users\ada\.hermes`, map[string]int{"": 1}},
		{`D:\Users\ada\.hermes\scripts and C:\Users\ada\.hermes-old\scripts`, nil},
	}
	for _, c := range found {
		if got := h.count(c.text); !maps.Equal(got, c.want) {
			t.Errorf("%q: found %v, want %v", c.text, got, c.want)
		}
	}
	rw := &rewriter{h: h, to: map[string]string{
		"scripts":    `C:\Users\ada\.habakkuk\files\scripts`,
		"state":      `C:\Users\ada\.habakkuk\files\state`,
		"workspaces": `D:\data\workspaces`,
	}}
	rewritten := []struct{ in, want string }{
		{`python C:\Users\ada\.hermes\scripts\sync.py`, `python C:\Users\ada\.habakkuk\files\scripts\sync.py`},
		// The separators the text used are kept, escaped ones too.
		{`python C:/Users/ada/.hermes/scripts/sync.py`, `python C:/Users/ada/.habakkuk/files/scripts/sync.py`},
		{`{"state": "C:\\Users\\ada\\.hermes\\state\\inbox.json"}`, `{"state": "C:\\Users\\ada\\.habakkuk\\files\\state\\inbox.json"}`},
		{`dir %USERPROFILE%\.hermes\scripts`, `dir %USERPROFILE%\.habakkuk\files\scripts`},
		{`ls ~\.hermes\scripts`, `ls ~\.habakkuk\files\scripts`},
		// A full path stays a full path: no "~" appears where none was, and
		// one that was there goes when the new place is on another drive.
		{`type C:\Users\ada\.hermes\state\x.json`, `type C:\Users\ada\.habakkuk\files\state\x.json`},
		{`ls ~\.hermes\workspaces\briefs`, `ls D:\data\workspaces\briefs`},
		{`dir %USERPROFILE%\.hermes\workspaces`, `dir D:\data\workspaces`},
	}
	for _, c := range rewritten {
		if got, _ := rw.apply(c.in); got != c.want {
			t.Errorf("%q\n  became %q\n  want   %q", c.in, got, c.want)
		}
	}
}

func TestHomeRefsNetworkShareAndRoot(t *testing.T) {
	h := newHomeRefs(`\\nas\home\ada\.hermes`, "", nil, true)
	if got := h.count(`copy \\nas\home\ada\.hermes\scripts\a.cmd`); got["scripts"] != 1 {
		t.Errorf("share path: found %v", got)
	}
	rw := &rewriter{h: h, to: map[string]string{"scripts": `\\nas\home\ada\habakkuk\files\scripts`}}
	if got := rw.str(`copy \\nas\home\ada\.hermes\scripts\a.cmd`); got != `copy \\nas\home\ada\habakkuk\files\scripts\a.cmd` {
		t.Errorf("share path became %q", got)
	}
	// A home that is the file system root names nothing.
	if got := newHomeRefs("/", "/root", nil, false).count("ls /etc /scripts"); got != nil {
		t.Errorf("root home: found %v", got)
	}
}
