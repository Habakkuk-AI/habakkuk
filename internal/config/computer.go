package config

import (
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Computer configures the computer toolset (agents opt in by listing the
// toolset "computer"): the agent sees a desktop through screenshots and
// drives it with the mouse and keyboard.
type Computer struct {
	// Backend is linux (this machine's X11 or Wayland screen), macos (this
	// Mac's screen) or xvfb (a private virtual desktop per agent on Linux).
	// Default macos on a Mac, linux elsewhere.
	Backend string `yaml:"backend"`
	// Display is the X display to drive (linux; default $DISPLAY), or the
	// first display number to give a virtual desktop (xvfb; default :99).
	Display string `yaml:"display"`
	// Screen is a virtual desktop's size, WIDTHxHEIGHT (xvfb; default
	// 1280x800).
	Screen string `yaml:"screen"`
	// MaxActions caps the computer actions (screenshots included, waits
	// not) one turn may take (default 150; -1 for no cap).
	MaxActions int `yaml:"max_actions"`
	// ConfirmApps always need a human's approval, in any permission mode:
	// an action while one of these apps is in front, or opening one.
	// Matched without case against the app's name or its window title.
	ConfirmApps []string `yaml:"confirm_apps"`
	// Native offers the provider's own computer-use tool instead of the
	// computer_* tools when every model in the agent's chain supports it
	// (default true).
	Native *bool `yaml:"native"`
	// Timeout bounds one action's commands (default 30s).
	Timeout time.Duration `yaml:"timeout"`
}

// ComputerConfig is the computer section with defaults applied.
func (c *Config) ComputerConfig() Computer {
	var k Computer
	if c.Computer != nil {
		k = *c.Computer
	}
	if k.Backend == "" {
		k.Backend = "linux"
		if runtime.GOOS == "darwin" {
			k.Backend = "macos"
		}
	}
	if k.Backend == "xvfb" {
		if k.Display == "" {
			k.Display = ":99"
		}
		if k.Screen == "" {
			k.Screen = "1280x800"
		}
	}
	if k.MaxActions == 0 {
		k.MaxActions = 150
	}
	if k.Native == nil {
		on := true
		k.Native = &on
	}
	if k.Timeout == 0 {
		k.Timeout = 30 * time.Second
	}
	return k
}

var (
	screenSize = regexp.MustCompile(`^(\d{3,5})x(\d{3,5})$`)
	xDisplay   = regexp.MustCompile(`^[A-Za-z0-9.\-]*:\d+(\.\d+)?$`)
)

// ParseScreen reads WIDTHxHEIGHT.
func ParseScreen(s string) (w, h int, err error) {
	m := screenSize.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, fmt.Errorf("screen %q must be WIDTHxHEIGHT, e.g. 1280x800", s)
	}
	w, _ = strconv.Atoi(m[1])
	h, _ = strconv.Atoi(m[2])
	if w < 320 || h < 200 || w > 7680 || h > 4320 {
		return 0, 0, fmt.Errorf("screen %q must be between 320x200 and 7680x4320", s)
	}
	return w, h, nil
}

func (c *Config) validateComputer() []error {
	k := c.Computer
	if k == nil {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("computer: "+format, args...)) }
	switch k.Backend {
	case "", "linux", "macos":
		if k.Screen != "" {
			bad("screen applies to backend xvfb only")
		}
	case "xvfb":
		if k.Screen != "" {
			if _, _, err := ParseScreen(k.Screen); err != nil {
				bad("%v", err)
			}
		}
	default:
		bad("backend %q must be linux, macos or xvfb", k.Backend)
	}
	if k.Display != "" {
		if k.Backend == "macos" {
			bad("display does not apply to backend macos")
		} else if !xDisplay.MatchString(k.Display) {
			bad("display %q must look like :99", k.Display)
		}
	}
	if k.MaxActions < -1 {
		bad("max_actions must be positive, or -1 for no cap")
	}
	if k.Timeout < 0 {
		bad("timeout must not be negative")
	}
	for _, a := range k.ConfirmApps {
		if strings.TrimSpace(a) == "" {
			bad("empty name in confirm_apps")
		}
	}
	return errs
}
