---
title: Sessions and chat commands
nav_title: Sessions
description: "How conversations map to sessions: idle timeouts, titles, compaction, fork and rewind, standing goals, export and search, and the chat commands that steer a turn."
order: 8
---

A conversation maps to a session: the history the model sees. A chat channel starts a fresh session after `session_idle` of silence (default 24 hours; the web chat never), or when you send `/new`.

## Steering a turn

| Command | What it does |
|---|---|
| `/stop` | Stop the turn in progress |
| `/retry` | Run your last message again |
| `/steer <text>` | Guidance for the task in progress, taken in before the next step |
| `/queue <text>` | A message to start on once the current task is done |
| `/btw <question>` | A side question, answered without adding it to the conversation |
| `/think low\|medium\|high\|xhigh\|max\|default` | The reasoning effort for this conversation |
| `/model [provider/model\|default]` | Show or switch the model for this conversation |
| `/personality <name>\|off` | Switch persona for this conversation |
| `/undo` | Put back the files the latest turn changed; repeat to go further back |

The [CLI reference](../reference/cli.md#chat-commands) lists every chat command.

## Standing goals

`/goal [-turns N] <text>` keeps the agent working on a goal turn after turn (default 10 turns, at most 100) until it replies that the goal is done or blocked. `/goal` shows the goal and the turns used; `/goal off` stops it after the current step. If the agent needs you, the goal pauses; answer, then send `/goal` again to resume.

## Titles

A session's title starts as its first message. After the first turn, a short title is written in the background by `title_model` (default the agent's `fast_model`). `/title [text]` shows or sets it.

## Compaction

Long sessions are compacted without rewriting anything: older messages are summarized into a record, and the model is sent the summary plus the recent turns. The full transcript stays for `habakkuk show`.

- It happens on its own once a prompt reaches the agent's `compact_at` (default 600000 tokens on `anthropic` providers, 150000 otherwise; `-1` turns it off).
- `/compact` or `habakkuk sessions compact ID` does it now.

## Fork and rewind

- `/fork` copies the session into a new one, to try another direction.
- `/rewind [N]` does the same without your last N messages (default 1).

The original stays as it was in both cases.

## Export and search

- `/export` sends the transcript as a Markdown file.
- `habakkuk sessions export ID -format md|json|html [-o FILE]` writes it from the terminal.
- `habakkuk sessions search [-agent A] QUERY` searches every session.
- `habakkuk sessions archive|unarchive|rm ID...` manages them; `habakkuk sessions archived` lists archived ones.
- The `session_search` tool lets an agent search its own past sessions.

```bash
habakkuk sessions               # recent sessions
habakkuk show <session>         # print a transcript
```

## Sub-agents

`/subagents` lists the sub-agent tasks running for this conversation, and `/subagents stop ID` cancels one. See [Delegation](multi-agent.md#delegation).

[Architecture: compaction](../reference/architecture.md#compaction) and [session tools](../reference/architecture.md#session-tools) have the details.
