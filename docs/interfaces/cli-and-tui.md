---
title: CLI and TUI
description: "Chat with an agent in the terminal: the full-screen UI, its keys and commands, approvals, plain line-by-line chat, and one-shot runs from scripts."
order: 1
---

Habakkuk works from a terminal too. Use `hk chat` when you want to talk to an agent. Use `hk run` when a script or cron job needs one answer.

## The terminal UI

```bash
hk chat                           # the terminal UI on a terminal; line by line when piped
hk chat -agent research           # another agent
hk chat -resume                   # pick a recent session
hk chat -session ID               # continue a given session
hk tui                            # always the full-screen UI
```

`hk chat` runs turns in your terminal for as long as it's open, so it doesn't need `hk serve`. Each tool call shows as a card you can expand. You can show or hide the model's thinking. Your first conversation with a new agent opens with its [welcome](../agents/agents.md#the-first-chat).

### Keys

| Key | What it does |
|---|---|
| Enter | Send |
| Alt+Enter, Shift+Enter or Ctrl+J | New line |
| Tab | Complete a command or its argument |
| Up / Down | Recall earlier messages |
| Esc Esc | Edit and resend the last message |
| Ctrl+C | Stop the running turn; twice to quit |
| PgUp / PgDn, mouse wheel | Scroll; click a tool card or thinking to expand it |
| Ctrl+O | Expand all tool output |
| Ctrl+T | Show all thinking |

Does your message begin with `/`? Start it with `//` and it's sent literally.

### Commands

| Command | What it does |
|---|---|
| `/help` | Commands and keys |
| `/new` | Start a fresh session with this agent |
| `/clear` | Clear the screen (the session is kept) |
| `/sessions` | Switch to a recent session |
| `/agent [name]`, `/agents` | Talk to another agent in a fresh session; list the agents |
| `/model [provider/model\|default]` | Show or switch this session's model |
| `/usage`, `/status` | Tokens and cost; agent, model, session and usage |
| `/retry`, `/stop` | Run your last message again; stop the running turn |
| `/btw`, `/steer`, `/queue` | A side question; guidance for the task in progress; a message for after it |
| `/think [level]`, `/personality [name\|off]` | Thinking effort and persona for this session |
| `/compact`, `/undo` | Summarize older messages; put back the files the last turn changed |
| `/export [path]` | Write this session as a Markdown file |
| `/quit`, `/exit` | Leave (a running turn finishes on the next start) |

### Approvals

When a call needs your approval, the UI shows you the request. You have three choices: **allow once** (`a` or `y`), **allow for this session** (`s`), or **deny** (`d`, `n` or Esc). You can also choose with the arrow keys and Enter.

### Plain chat

`hk chat -plain` gives you a line-by-line chat with no full-screen UI. You get the same thing from any `hk chat` whose input is piped. It suits scripts and dumb terminals.

## One-shot runs

```bash
hk run "summarise the files in my workspace"              # one turn, print the answer, exit
hk run -agent research "compare these two papers"
hk run -skill weekly-report                                # follow a skill
```

`hk run` prints the answer and exits, which makes it a good fit for scripts and cron jobs. There's nobody at the keyboard to approve anything, so a call that needs approval is denied. The exception is an agent that has an [approval reviewer](../agents/approvals.md#approvals).

## Everything else

For every command and flag, see the [CLI reference](../reference/cli.md). You get the same list when you run `habakkuk` with no arguments.
