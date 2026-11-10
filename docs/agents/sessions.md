---
title: Sessions and chat commands
nav_title: Sessions
description: "How conversations map to sessions: idle timeouts, titles, compaction, fork and rewind, standing goals, export and search, and the chat commands that steer a turn."
order: 8
---

A conversation maps to a session: the history the model sees. A chat channel starts a fresh session after `session_idle` of silence (default 24 hours; the web chat never), or when you send `/new`.

## Messages from elsewhere in the same chat

A chat's session holds the turns that chat started. Other messages land in the same chat too:

- the result of a [scheduled task](schedules.md), which runs in its own session, including a script schedule with `-on-output deliver` or `agent`;
- another agent's report or `message_home` post, when this chat is its [home chat](multi-agent.md#teams-and-home-chats);
- a background task's result, a board update, `habakkuk send` and the MCP `habakkuk_send` tool.

Habakkuk keeps a note of each one, in `state.db`, for the chat it went to: the same channel account, chat and thread. On your next message, the agent that answers the chat sees the notes it has not seen yet before your words, like this:

```text
[Earlier in this chat, at 18:44, your scheduled task "Inbox check" sent: "3 new emails, 1 needs an answer today."]
[Earlier in this chat, at 18:05, agent engineering sent: "Tests pass. Merge it?"]
how did you do that?
```

So "yes" or "how did you do that?" is read against the message you are answering, even when that message came from the agent's own scheduled run.

- **Once each.** A note is shown to one turn, then marked seen. If that turn crashes before its message is saved, the retry shows the same notes, so none is lost and none appears twice.
- **Never mid-turn.** Notes join your next message. They are never inserted into a turn that is running.
- **Bounded.** Each note is cut to 1,200 characters and a turn takes at most 6,000 characters of notes. The newest are kept, and a line says how many older ones were left out.
- **Not the chat's own replies.** What the chat's own session sent is already in its history and leaves no note.

Notes are pruned with the rest of `state.db`, following transcript retention.

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
