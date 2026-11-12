package cutover

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The service managers an old agent can be installed under.
const (
	Systemd       = "systemd"        // a systemd user unit
	SystemdSystem = "systemd-system" // a system-wide unit: changing it needs root
	Launchd       = "launchd"        // a macOS launchd agent
	Schtasks      = "schtasks"       // a Windows scheduled task
	Startup       = "startup"        // a launcher in the Windows Startup folder
)

// startupOff is the suffix a disabled Startup-folder launcher gets.
const startupOff = ".disabled-by-habakkuk"

// Service is one background service the old agent installed.
type Service struct {
	Manager string `json:"manager"`
	// Name is the unit (hermes-gateway.service), launchd label or task name.
	Name string `json:"name"`
	// File is the unit file or plist, when there is one.
	File string `json:"file,omitempty"`
	// What it runs, in a few words: "gateway".
	Role string `json:"role,omitempty"`
}

// ServiceState is what the service manager says about a service.
type ServiceState struct {
	Active  bool `json:"active"`  // running now
	Enabled bool `json:"enabled"` // starts by itself (at login or boot)
}

func (s Service) String() string {
	switch s.Manager {
	case Systemd:
		return "systemd " + strings.TrimSuffix(s.Name, ".service")
	case SystemdSystem:
		return "systemd system unit " + strings.TrimSuffix(s.Name, ".service")
	case Launchd:
		return "launchd " + s.Name
	case Schtasks:
		return "scheduled task " + s.Name
	case Startup:
		return "Startup folder " + s.Name
	}
	return s.Name
}

// NeedsRoot reports whether only an administrator can stop it.
func (s Service) NeedsRoot() bool { return s.Manager == SystemdSystem }

// StopCommand is the command that stops and disables the service, for
// people to run themselves.
func (s Service) StopCommand(uid int) string {
	switch s.Manager {
	case Systemd:
		return "systemctl --user disable --now " + s.Name
	case SystemdSystem:
		return "sudo systemctl disable --now " + s.Name
	case Launchd:
		return fmt.Sprintf("launchctl bootout gui/%d/%s && launchctl disable gui/%d/%s", uid, s.Name, uid, s.Name)
	case Schtasks:
		return fmt.Sprintf(`schtasks /End /TN "%s" & schtasks /Change /TN "%s" /DISABLE`, s.Name, s.Name)
	case Startup:
		return "move " + s.File + " out of the Startup folder"
	}
	return ""
}

// state asks the service manager about a service. ok is false when the
// manager does not know it (not installed, or not loaded and no file).
func state(ctx context.Context, sys System, s Service) (st ServiceState, ok bool) {
	switch s.Manager {
	case Systemd, SystemdSystem:
		args := []string{"--user"}
		if s.Manager == SystemdSystem {
			args = nil
		}
		active, _ := sys.Run(ctx, "systemctl", append(args, "is-active", s.Name)...)
		enabled, _ := sys.Run(ctx, "systemctl", append(args, "is-enabled", s.Name)...)
		active, enabled = firstLine(active), firstLine(enabled)
		st.Active = active == "active" || active == "activating" || active == "reloading"
		st.Enabled = enabled == "enabled" || enabled == "enabled-runtime" || enabled == "linked"
		return st, st.Active || st.Enabled || (enabled != "" && enabled != "not-found")
	case Launchd:
		out, err := sys.Run(ctx, "launchctl", "print", launchdTarget(sys, s))
		loaded := err == nil
		st.Active = loaded && strings.Contains(out, "state = running")
		dis, _ := sys.Run(ctx, "launchctl", "print-disabled", "gui/"+strconv.Itoa(sys.UID()))
		// A loaded agent, or an installed plist that is not disabled,
		// starts again at the next login.
		st.Enabled = !launchdDisabled(dis, s.Name) && (loaded || s.File != "")
		return st, loaded || s.File != ""
	case Schtasks:
		out, err := sys.Run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(Get-ScheduledTask -TaskName '"+psQuote(s.Name)+"' -ErrorAction Stop).State")
		if err != nil {
			return st, false
		}
		switch strings.TrimSpace(firstLine(out)) {
		case "Running":
			st.Active, st.Enabled = true, true
		case "Ready", "Queued":
			st.Enabled = true
		}
		return st, true
	case Startup:
		st.Enabled = fileExists(s.File)
		return st, st.Enabled
	}
	return st, false
}

// stopService stops a service and keeps it from starting again.
func stopService(ctx context.Context, sys System, s Service) error {
	var err error
	switch s.Manager {
	case Systemd:
		_, err = sys.Run(ctx, "systemctl", "--user", "disable", "--now", s.Name)
	case SystemdSystem:
		err = fmt.Errorf("it is a system unit; run %s", s.StopCommand(sys.UID()))
	case Launchd:
		target := launchdTarget(sys, s)
		// bootout fails when it is not loaded, which is the aim anyway.
		if _, e := sys.Run(ctx, "launchctl", "print", target); e == nil {
			if _, err = sys.Run(ctx, "launchctl", "bootout", target); err != nil {
				return err
			}
		}
		_, err = sys.Run(ctx, "launchctl", "disable", target)
	case Schtasks:
		_, _ = sys.Run(ctx, "schtasks", "/End", "/TN", s.Name) // fails when it is not running
		_, err = sys.Run(ctx, "schtasks", "/Change", "/TN", s.Name, "/DISABLE")
	case Startup:
		// Renamed, not removed: Windows runs only the launchers it knows,
		// and rollback names it back.
		err = os.Rename(s.File, s.File+startupOff)
	}
	return err
}

// restoreService puts a service back the way it was.
func restoreService(ctx context.Context, sys System, s Service, was ServiceState) error {
	var err error
	switch s.Manager {
	case Systemd:
		if was.Enabled {
			if _, err = sys.Run(ctx, "systemctl", "--user", "enable", s.Name); err != nil {
				return err
			}
		}
		if was.Active {
			_, err = sys.Run(ctx, "systemctl", "--user", "start", s.Name)
		}
	case Launchd:
		// Enabled again only if it was before: finish disables it, and an
		// agent you had turned off stays off.
		target := launchdTarget(sys, s)
		if was.Enabled {
			if _, err = sys.Run(ctx, "launchctl", "enable", target); err != nil {
				return err
			}
		}
		if was.Active && s.File != "" {
			_, err = sys.Run(ctx, "launchctl", "bootstrap", "gui/"+strconv.Itoa(sys.UID()), s.File)
		}
	case Schtasks:
		if was.Enabled {
			if _, err = sys.Run(ctx, "schtasks", "/Change", "/TN", s.Name, "/ENABLE"); err != nil {
				return err
			}
		}
		if was.Active {
			_, err = sys.Run(ctx, "schtasks", "/Run", "/TN", s.Name)
		}
	case Startup:
		if fileExists(s.File + startupOff) {
			err = os.Rename(s.File+startupOff, s.File)
		}
	}
	return err
}

func launchdTarget(sys System, s Service) string {
	return "gui/" + strconv.Itoa(sys.UID()) + "/" + s.Name
}

// launchdDisabled reads launchctl print-disabled: lines such as
// `"ai.example.gateway" => disabled` (older macOS: `=> true`).
func launchdDisabled(out, label string) bool {
	for _, line := range strings.Split(out, "\n") {
		name, val, ok := strings.Cut(strings.TrimSpace(line), "=>")
		if !ok || strings.Trim(strings.TrimSpace(name), `"`) != label {
			continue
		}
		val = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(val), ","))
		return val == "disabled" || val == "true"
	}
	return false
}

// psQuote escapes a value for a single-quoted PowerShell string.
func psQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }
