package platform

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

// Command strings (hooks, scheduled scripts, secret commands, the exec tool)
// run through a shell:
//
//   - Unix: /bin/sh -c, or bash -c for the agent's exec tool.
//   - Windows: PowerShell (pwsh when installed, else Windows PowerShell),
//     because agents and users write it far more reliably than cmd.exe
//     batch syntax and it has ls, cat, rm and friends as aliases.
//
// $HABAKKUK_SHELL picks another one: "cmd", "powershell", "pwsh", "bash" or
// "sh" (bash or sh found on PATH, e.g. Git for Windows), or a path to one of
// those programs.

// ShellEnv names the variable that overrides the shell.
const ShellEnv = "HABAKKUK_SHELL"

// shellSpec is how to start a shell running one script.
type shellSpec struct {
	kind    string // sh, bash, powershell, pwsh, cmd
	path    string
	args    []string
	cmdLine string // Windows only: the raw command line (cmd.exe has its own quoting)
}

// psPrelude makes PowerShell quiet and UTF-8, as callers read its output as
// text.
const psPrelude = "$ProgressPreference='SilentlyContinue'; [Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false)\n"

// psEpilogue gives PowerShell sh-like exit codes: the failing native
// command's code, or 1 when the last statement failed.
const psEpilogue = "\n$__habakkukOK = $?; if (-not $__habakkukOK) { if ($LASTEXITCODE) { exit $LASTEXITCODE } else { exit 1 } }"

// maxEncoded keeps -EncodedCommand within Windows' 32767-character command
// line, with room for the program path and flags.
const maxEncoded = 30000

func shellFor(goos, pref, script string, wantBash bool, look func(string) (string, error)) shellSpec {
	kind := strings.ToLower(strings.TrimSuffix(filepath.Base(pref), filepath.Ext(pref)))
	path := pref
	if pref == "" || !strings.ContainsAny(pref, `/\`) {
		path = ""
	}
	if goos != "windows" {
		switch {
		case kind == "pwsh" || kind == "powershell":
		case kind == "sh" || kind == "bash" || kind == "zsh" || kind == "dash":
		case path != "":
			// A path to a shell this doesn't know (fish, a wrapper): it runs,
			// with -c, under its own name, never labelled bash or sh.
		case wantBash:
			kind = "bash"
		default:
			kind = "sh"
		}
		if kind == "sh" && path == "" {
			path = "/bin/sh"
		}
		if kind == "bash" && path == "" {
			if p, err := look("bash"); err == nil {
				path = p
			} else {
				kind, path = "sh", "/bin/sh"
			}
		}
	} else {
		switch kind {
		case "cmd", "powershell", "pwsh", "bash", "sh":
		default:
			kind = ""
		}
		if kind == "" {
			kind = "powershell"
			if p, err := look("pwsh"); err == nil {
				kind, path = "pwsh", p
			}
		}
		if path == "" {
			switch kind {
			case "cmd":
				path = comspec()
			case "powershell":
				path = windowsPowerShell()
			default:
				if p, err := look(kind); err == nil {
					path = p
				} else {
					kind, path = "powershell", windowsPowerShell()
				}
			}
		}
	}
	if path == "" {
		if p, err := look(kind); err == nil {
			path = p
		} else {
			path = kind
		}
	}
	s := shellSpec{kind: kind, path: path}
	switch kind {
	case "powershell", "pwsh":
		body := psPrelude + script + psEpilogue
		s.args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"}
		if enc := encodePS(body); len(enc) <= maxEncoded {
			s.args = append(s.args, "-EncodedCommand", enc)
		} else {
			s.args = append(s.args, "-Command", body)
		}
	case "cmd":
		// /s strips the outer quotes and keeps everything inside as typed.
		s.args = []string{"/d", "/s", "/c", script}
		s.cmdLine = `"` + path + `" /d /s /c "` + script + `"`
	default:
		s.args = []string{"-c", script}
	}
	return s
}

// encodePS encodes a script for powershell -EncodedCommand: base64 of its
// UTF-16LE bytes, which sidesteps Windows command-line quoting entirely.
func encodePS(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(u)*2)
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func comspec() string {
	if c := os.Getenv("ComSpec"); c != "" {
		return c
	}
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "System32", "cmd.exe")
	}
	return "cmd.exe"
}

func windowsPowerShell() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	}
	return "powershell.exe"
}

func (s shellSpec) cmd() *exec.Cmd {
	cmd := exec.Command(s.path, s.args...)
	if s.cmdLine != "" {
		setCmdLine(cmd, s.cmdLine)
	}
	return cmd
}

func (s shellSpec) cmdContext(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, s.path, s.args...)
	if s.cmdLine != "" {
		setCmdLine(cmd, s.cmdLine)
	}
	return cmd
}

func userShell(script string) shellSpec {
	return shellFor(runtime.GOOS, os.Getenv(ShellEnv), script, false, exec.LookPath)
}

func scriptShell(script string) shellSpec {
	return shellFor(runtime.GOOS, os.Getenv(ShellEnv), script, true, exec.LookPath)
}

// ShellCommand prepares the shell running script, for user-configured
// commands (hooks, scheduled scripts, secret commands): /bin/sh on Unix,
// PowerShell on Windows.
func ShellCommand(script string) *exec.Cmd { return userShell(script).cmd() }

// ShellCommandContext is ShellCommand bound to ctx like exec.CommandContext.
func ShellCommandContext(ctx context.Context, script string) *exec.Cmd {
	return userShell(script).cmdContext(ctx)
}

// ShellArgv returns the program and arguments ShellCommand would run, for
// callers that start processes through their own runner.
func ShellArgv(script string) (string, []string) {
	s := userShell(script)
	return s.path, s.args
}

// CommandInDir returns command prefixed so that ShellCommand runs it in dir:
// cd 'dir' && ... for sh, Set-Location -LiteralPath 'dir'; ... for
// PowerShell, cd /d "dir" && ... for cmd.
func CommandInDir(dir, command string) string {
	return commandInDir(userShell("").kind, dir, command)
}

func commandInDir(kind, dir, command string) string {
	switch kind {
	case "powershell", "pwsh":
		return "Set-Location -LiteralPath '" + strings.ReplaceAll(dir, "'", "''") + "'; " + command
	case "cmd":
		return `cd /d "` + dir + `" && ` + command
	}
	return "cd '" + strings.ReplaceAll(dir, "'", `'\''`) + "' && " + command
}

// MinimalEnvKeys are the environment variables passed to child programs
// that get a clean environment (MCP servers, plugin scripts): enough to find
// programs and a home and temp folder, nothing secret. Windows programs also
// need SystemRoot, APPDATA and friends to start at all.
func MinimalEnvKeys() []string {
	if IsWindows {
		return []string{"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "windir", "ComSpec", "USERPROFILE",
			"HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "ProgramData", "ProgramFiles", "ProgramFiles(x86)",
			"TEMP", "TMP", "USERNAME", "COMPUTERNAME", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE"}
	}
	return []string{"PATH", "HOME", "LANG", "TMPDIR", "USER"}
}

// ScriptCommand prepares the shell the agent's exec tool uses: bash on Unix
// (falling back to sh), PowerShell on Windows.
func ScriptCommand(script string) *exec.Cmd { return scriptShell(script).cmd() }

// ScriptShellName names the shell ScriptCommand uses, for tool descriptions
// and prompts: "bash", "sh", "zsh", "dash", "PowerShell", "cmd", or the
// name of another program HABAKKUK_SHELL points to.
func ScriptShellName() string {
	switch k := scriptShell("").kind; k {
	case "powershell", "pwsh":
		return "PowerShell"
	default:
		return k
	}
}
