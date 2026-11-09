---
title: CLI and TUI
description: "Chat with an agent in the full-screen terminal UI, its keys and commands, approvals at the keyboard, plain line-by-line chat, and one-shot runs from scripts."
order: 1
---

## The terminal UI

```bash
habakkuk chat                     # the terminal UI on a terminal; line by line when piped
habakkuk chat -agent research     # another agent
habakkuk chat -resume             # pick a recent session
habakkuk chat -session ID         # continue a given session
habakkuk tui                      # always the full-screen UI
```

`habakkuk chat` runs turns in the terminal while it is open; it does not need `habakkuk serve`. Tool calls show as cards you can expand, and the model's thinking can be shown or hidden. The first conversation with a new agent opens with its [welcome](../agents/agents.md#the-first-chat).

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

Start a message with `//` to send it literally when it begins with `/`.

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

When a call needs approval, the UI shows the request with three choices: **allow once** (`a` or `y`), **allow for this session** (`s`), or **deny** (`d`, `n` or Esc). Arrow keys and Enter work too.

### Plain chat

`habakkuk chat -plain` (or any `habakkuk chat` whose input is piped) is a line-by-line chat with no full-screen UI, which suits scripts and dumb terminals.

## One-shot runs

```bash
habakkuk run "summarise the files in my workspace"        # one turn, print the answer, exit
habakkuk run -agent research "compare these two papers"
habakkuk run -skill weekly-report                          # follow a skill
```

`habakkuk run` prints the answer and exits, so it fits scripts and cron jobs. Nobody is there to approve, so calls that need approval are denied unless the agent has an [approval reviewer](../agents/approvals.md#approvals).

## Everything else

The [CLI reference](../reference/cli.md) lists every command and flag. `habakkuk` with no arguments prints the same list.
