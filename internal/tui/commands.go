package tui

import (
	"slices"
	"strings"
)

// command is a slash command. Local ones the UI runs itself; backend ones
// go to Backend.Command.
type command struct {
	name    string
	aliases []string
	args    string // shown in help and the popup, e.g. "<question>"
	help    string
	backend bool
	// choices completes the argument, when it is one of a few words.
	choices func(m *Model) []string
}

// Efforts are the levels /think takes.
var efforts = []string{"low", "medium", "high", "xhigh", "max", "default"}

var commands = []command{
	{name: "/help", help: "commands and keys"},
	{name: "/new", help: "start a fresh session with this agent"},
	{name: "/clear", help: "clear the screen (the session is kept)"},
	{name: "/sessions", help: "switch to a recent session"},
	{name: "/agent", args: "[name]", help: "talk to another agent, in a fresh session", choices: func(m *Model) []string { return m.be.Agents() }},
	{name: "/agents", help: "list the agents"},
	{name: "/model", args: "[provider/model|default]", help: "show or switch this session's model", backend: true},
	{name: "/usage", help: "tokens and cost of this session", backend: true},
	{name: "/retry", help: "run my last message again", backend: true},
	{name: "/stop", help: "stop the running turn (also Ctrl+C)"},
	{name: "/btw", args: "<question>", help: "a side question, answered without adding it to this session", backend: true},
	{name: "/steer", args: "<text>", help: "guidance for the task in progress, taken in before the next step", backend: true},
	{name: "/queue", args: "<text>", help: "a message to start on once the current turn is done"},
	{name: "/think", args: "[level]", help: "how hard the agent thinks in this session", backend: true, choices: func(*Model) []string { return efforts }},
	{name: "/personality", aliases: []string{"/persona"}, args: "[name|off]", help: "switch the persona for this session", backend: true},
	{name: "/compact", help: "summarize the older part of this session to free up room", backend: true},
	{name: "/undo", help: "put back the files the last turn changed", backend: true},
	{name: "/export", args: "[path]", help: "write this session as a Markdown file"},
	{name: "/status", help: "agent, model, session and usage"},
	{name: "/quit", aliases: []string{"/exit"}, help: "leave (a running turn finishes on the next start)"},
}

// CommandNames are the slash commands the terminal UI runs itself, with
// their aliases.
func CommandNames() []string {
	var out []string
	for _, c := range commands {
		out = append(out, c.name)
		out = append(out, c.aliases...)
	}
	return out
}

func lookupCommand(name string) (command, bool) {
	name = strings.ToLower(name)
	for _, c := range commands {
		if c.name == name || slices.Contains(c.aliases, name) {
			return c, true
		}
	}
	return command{}, false
}

// completion is one popup row: the input it fills in, and a hint.
type completion struct {
	value, label, help string
	run                bool // the command takes no argument, so Enter runs it
}

// complete lists completions for the input: command names while the first
// word is typed, then the argument's choices.
func (m *Model) complete(v string) []completion {
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, "\n") {
		return nil
	}
	name, arg, spaced := strings.Cut(v, " ")
	var out []completion
	if !spaced {
		low := strings.ToLower(name)
		for _, c := range commands {
			names := append([]string{c.name}, c.aliases...)
			for _, n := range names {
				if strings.HasPrefix(n, low) {
					label := n
					if c.args != "" {
						label += " " + c.args
					}
					value := n
					if c.args != "" {
						value += " "
					}
					out = append(out, completion{value: value, label: label, help: c.help, run: optionalArgs(c)})
					break
				}
			}
		}
		if c, ok := lookupCommand(low); ok && optionalArgs(c) && len(out) == 1 {
			return nil // typed in full; Enter runs it
		}
		return out
	}
	c, ok := lookupCommand(name)
	if !ok || c.choices == nil || strings.Contains(arg, " ") {
		return nil
	}
	for _, ch := range c.choices(m) {
		if strings.HasPrefix(ch, arg) && ch != arg {
			out = append(out, completion{value: name + " " + ch, label: ch, run: true})
		}
	}
	return out
}

// optionalArgs reports whether the command runs without an argument.
func optionalArgs(c command) bool { return c.args == "" || strings.HasPrefix(c.args, "[") }

func helpText() string {
	var b strings.Builder
	b.WriteString("Commands:")
	for _, c := range commands {
		b.WriteString("\n  " + c.name)
		if c.args != "" {
			b.WriteString(" " + c.args)
		}
		b.WriteString("  " + c.help)
	}
	b.WriteString(`

Keys:
  Enter send · Alt+Enter or Ctrl+J new line · Tab complete
  Up/Down recall earlier messages · Esc Esc edit and resend the last message
  Ctrl+C stop the running turn, twice to quit
  PgUp/PgDn or the mouse wheel scroll · click a tool card or thinking to expand it
  Ctrl+O expand all tool output · Ctrl+T show all thinking
  Start a message with // to send it literally when it begins with /`)
	return b.String()
}
