package computer

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// combo is a parsed key combination: modifiers (ctrl, alt, shift, super)
// and at most one other key, by a normalized name ("return", "pageup",
// "f5") or as a single character.
type combo struct {
	mods []string
	key  string
}

var modNames = map[string]string{
	"ctrl": "ctrl", "control": "ctrl", "ctl": "ctrl",
	"alt": "alt", "option": "alt", "opt": "alt", "mod1": "alt",
	"shift": "shift",
	"super": "super", "cmd": "super", "command": "super", "meta": "super", "win": "super", "windows": "super", "mod4": "super",
}

var keyNames = map[string]string{
	"enter": "return", "return": "return", "kp_enter": "return",
	"esc": "escape", "escape": "escape",
	"tab":       "tab",
	"backspace": "backspace", "back_space": "backspace",
	"delete": "delete", "del": "delete",
	"space": "space", "spacebar": "space",
	"up": "up", "arrowup": "up", "arrow_up": "up", "uparrow": "up",
	"down": "down", "arrowdown": "down", "arrow_down": "down", "downarrow": "down",
	"left": "left", "arrowleft": "left", "arrow_left": "left", "leftarrow": "left",
	"right": "right", "arrowright": "right", "arrow_right": "right", "rightarrow": "right",
	"home": "home", "end": "end",
	"pageup": "pageup", "page_up": "pageup", "prior": "pageup", "pgup": "pageup",
	"pagedown": "pagedown", "page_down": "pagedown", "next": "pagedown", "pgdn": "pagedown",
	"insert": "insert", "ins": "insert",
	"capslock": "capslock", "caps_lock": "capslock",
	"printscreen": "printscreen", "print": "printscreen",
	"menu": "menu",
}

// parseCombo reads a combination such as "ctrl+l", "Return", "cmd+shift+t"
// or "ctrl++". Names are matched without case; a key name it does not know
// is kept as given, for backends that take raw names (X keysyms).
func parseCombo(s string) (combo, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return combo{}, fmt.Errorf("empty key")
	}
	var parts []string
	if s == "+" {
		parts = []string{"+"}
	} else if strings.HasSuffix(s, "++") {
		parts = append(strings.Split(strings.TrimSuffix(s, "++"), "+"), "+")
	} else {
		parts = strings.Split(s, "+")
	}
	var c combo
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return combo{}, fmt.Errorf("key %q has an empty part", s)
		}
		if m, ok := modNames[strings.ToLower(p)]; ok {
			c.mods = append(c.mods, m) // last, it is pressed alone ("shift", "ctrl+shift")
			continue
		}
		if i != len(parts)-1 {
			return combo{}, fmt.Errorf("key %q: %q is not a modifier (ctrl, alt, shift, super)", s, p)
		}
		c.key = normalizeKey(p)
	}
	return c, nil
}

// parseMods reads modifiers alone ("shift", "ctrl+shift"), as held during a
// click or scroll.
func parseMods(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	c, err := parseCombo(s)
	if err != nil {
		return nil, err
	}
	if c.key != "" {
		return nil, fmt.Errorf("%q: only modifiers (ctrl, alt, shift, super) can be held", s)
	}
	return c.mods, nil
}

func normalizeKey(p string) string {
	if utf8.RuneCountInString(p) == 1 {
		return p
	}
	l := strings.ToLower(p)
	if k, ok := keyNames[l]; ok {
		return k
	}
	if n, ok := fKey(l); ok {
		return "f" + strconv.Itoa(n)
	}
	return p
}

// fKey reads f1 to f24.
func fKey(k string) (int, bool) {
	if len(k) < 2 || k[0] != 'f' {
		return 0, false
	}
	n, err := strconv.Atoi(k[1:])
	return n, err == nil && n >= 1 && n <= 24
}

// X keysyms for xdotool.
var xKeys = map[string]string{
	"return": "Return", "escape": "Escape", "tab": "Tab", "backspace": "BackSpace", "delete": "Delete",
	"space": "space", "up": "Up", "down": "Down", "left": "Left", "right": "Right", "home": "Home", "end": "End",
	"pageup": "Prior", "pagedown": "Next", "insert": "Insert", "capslock": "Caps_Lock", "printscreen": "Print", "menu": "Menu",
	" ": "space", "-": "minus", "=": "equal", "[": "bracketleft", "]": "bracketright", "\\": "backslash", ";": "semicolon",
	"'": "apostrophe", ",": "comma", ".": "period", "/": "slash", "`": "grave", "+": "plus", "!": "exclam", "@": "at",
	"#": "numbersign", "$": "dollar", "%": "percent", "^": "asciicircum", "&": "ampersand", "*": "asterisk",
	"(": "parenleft", ")": "parenright", "_": "underscore", "{": "braceleft", "}": "braceright", "|": "bar",
	":": "colon", "\"": "quotedbl", "<": "less", ">": "greater", "?": "question", "~": "asciitilde",
}

// xdoCombo is the combination as xdotool key takes it.
func xdoCombo(c combo) string {
	parts := append([]string{}, c.mods...)
	if c.key != "" {
		k := c.key
		if x, ok := xKeys[k]; ok {
			k = x
		} else if n, ok := fKey(k); ok {
			k = "F" + strconv.Itoa(n)
		}
		parts = append(parts, k)
	}
	return strings.Join(parts, "+")
}

// Linux input event codes for ydotool.
var evdevKeys = map[string]int{
	"escape": 1, "1": 2, "2": 3, "3": 4, "4": 5, "5": 6, "6": 7, "7": 8, "8": 9, "9": 10, "0": 11, "-": 12, "=": 13,
	"backspace": 14, "tab": 15, "q": 16, "w": 17, "e": 18, "r": 19, "t": 20, "y": 21, "u": 22, "i": 23, "o": 24, "p": 25,
	"[": 26, "]": 27, "return": 28, "a": 30, "s": 31, "d": 32, "f": 33, "g": 34, "h": 35, "j": 36, "k": 37, "l": 38,
	";": 39, "'": 40, "`": 41, "\\": 43, "z": 44, "x": 45, "c": 46, "v": 47, "b": 48, "n": 49, "m": 50, ",": 51, ".": 52,
	"/": 53, "space": 57, " ": 57, "capslock": 58, "f1": 59, "f2": 60, "f3": 61, "f4": 62, "f5": 63, "f6": 64, "f7": 65,
	"f8": 66, "f9": 67, "f10": 68, "f11": 87, "f12": 88, "printscreen": 99, "home": 102, "up": 103, "pageup": 104,
	"left": 105, "right": 106, "end": 107, "down": 108, "pagedown": 109, "insert": 110, "delete": 111, "menu": 139,
}

var evdevMods = map[string]int{"ctrl": 29, "shift": 42, "alt": 56, "super": 125}

// evdevCodes are a combination's key codes, modifiers first; an upper-case
// letter adds shift.
func evdevCodes(c combo) ([]int, error) {
	mods := c.mods
	key := c.key
	if len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z' {
		key = strings.ToLower(key)
		mods = append(append([]string{}, mods...), "shift")
	}
	var codes []int
	for _, m := range mods {
		codes = append(codes, evdevMods[m])
	}
	if key != "" {
		code, ok := evdevKeys[key]
		if !ok {
			return nil, fmt.Errorf("key %q is not supported on Wayland (ydotool)", c.key)
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// ydoKeyArgs presses the codes in order and releases them in reverse.
func ydoKeyArgs(codes []int, press, release bool) []string {
	var args []string
	if press {
		for _, c := range codes {
			args = append(args, strconv.Itoa(c)+":1")
		}
	}
	if release {
		for i := len(codes) - 1; i >= 0; i-- {
			args = append(args, strconv.Itoa(codes[i])+":0")
		}
	}
	return args
}

// cliclick names.
var cliKeys = map[string]string{
	"return": "return", "escape": "esc", "tab": "tab", "backspace": "delete", "delete": "fwd-delete", "space": "space",
	"up": "arrow-up", "down": "arrow-down", "left": "arrow-left", "right": "arrow-right", "home": "home", "end": "end",
	"pageup": "page-up", "pagedown": "page-down",
}

var cliMods = map[string]string{"ctrl": "ctrl", "alt": "alt", "shift": "shift", "super": "cmd"}

// cliclickKeyArgs presses a combination with cliclick: the modifiers held
// (kd/ku) around a key press (kp) or a typed character (t).
func cliclickKeyArgs(c combo) ([]string, error) {
	var mods []string
	for _, m := range c.mods {
		mods = append(mods, cliMods[m])
	}
	var args []string
	if len(mods) > 0 {
		args = append(args, "kd:"+strings.Join(mods, ","))
	}
	switch k := c.key; {
	case k == "":
	case utf8.RuneCountInString(k) == 1:
		args = append(args, "t:"+k)
	default:
		name, ok := cliKeys[k]
		if !ok {
			if n, isF := fKey(k); isF && n <= 16 {
				name, ok = k, true
			}
		}
		if !ok {
			return nil, fmt.Errorf("key %q is not supported on macOS", c.key)
		}
		args = append(args, "kp:"+name)
	}
	if len(mods) > 0 {
		args = append(args, "ku:"+strings.Join(mods, ","))
	}
	return args, nil
}

// macOS virtual key codes (ANSI layout), for AppleScript key code and
// keyboard events.
var macKeys = map[string]int{
	"return": 36, "tab": 48, "space": 49, " ": 49, "backspace": 51, "escape": 53, "delete": 117, "home": 115, "end": 119,
	"pageup": 116, "pagedown": 121, "left": 123, "right": 124, "down": 125, "up": 126,
	"f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97, "f7": 98, "f8": 100, "f9": 101, "f10": 109, "f11": 103, "f12": 111,
	"a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9, "b": 11, "q": 12, "w": 13, "e": 14,
	"r": 15, "y": 16, "t": 17, "1": 18, "2": 19, "3": 20, "4": 21, "6": 22, "5": 23, "=": 24, "9": 25, "7": 26, "-": 27,
	"8": 28, "0": 29, "]": 30, "o": 31, "u": 32, "[": 33, "i": 34, "p": 35, "l": 37, "j": 38, "'": 39, "k": 40, ";": 41,
	"\\": 42, ",": 43, "/": 44, "n": 45, "m": 46, ".": 47, "`": 50,
}

var macModKeys = map[string]int{"super": 55, "shift": 56, "alt": 58, "ctrl": 59}

var appleMods = map[string]string{"ctrl": "control down", "alt": "option down", "shift": "shift down", "super": "command down"}

// appleScriptKey presses a combination repeat times through System Events.
// A character is passed as the script's argument, never spliced into it.
func appleScriptKey(c combo, repeat int) (script string, args []string, err error) {
	var using []string
	for _, m := range c.mods {
		using = append(using, appleMods[m])
	}
	suffix := ""
	if len(using) > 0 {
		suffix = " using {" + strings.Join(using, ", ") + "}"
	}
	var press string
	switch k := c.key; {
	case k == "":
		if len(c.mods) == 0 {
			return "", nil, fmt.Errorf("empty key")
		}
		press = fmt.Sprintf("key code %d%s", macModKeys[c.mods[len(c.mods)-1]], suffix)
	case utf8.RuneCountInString(k) == 1:
		press, args = "keystroke (item 1 of argv)"+suffix, []string{k}
	default:
		code, ok := macKeys[k]
		if !ok {
			return "", nil, fmt.Errorf("key %q is not supported on macOS", c.key)
		}
		press = fmt.Sprintf("key code %d%s", code, suffix)
	}
	script = fmt.Sprintf("on run argv\ntell application \"System Events\"\nrepeat %d times\n%s\nend repeat\nend tell\nend run", max(repeat, 1), press)
	return script, args, nil
}

// macKeyCodes are a combination's key codes, modifiers first, for holding.
func macKeyCodes(c combo) ([]int, error) {
	var codes []int
	for _, m := range c.mods {
		codes = append(codes, macModKeys[m])
	}
	if c.key != "" {
		code, ok := macKeys[strings.ToLower(c.key)]
		if !ok {
			return nil, fmt.Errorf("key %q cannot be held on macOS", c.key)
		}
		codes = append(codes, code)
	}
	return codes, nil
}
