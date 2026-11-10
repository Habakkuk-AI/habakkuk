---
title: Sessions and chat commands
nav_title: Sessions
description: "How conversations become sessions, with idle timeouts, titles, compaction, fork and rewind, standing goals, export, search and the commands that steer a turn."
order: 8
---

Habakkuk keeps a history for each conversation, and a handful of chat commands let you steer a turn. Each conversation maps to a session, which is the history the model sees. A chat channel starts a fresh session after `session_idle` of silence. The default is 24 hours, and the web chat never starts one this way. A fresh session also starts when you send `/new`.

## Messages from elsewhere in the same chat

A chat's session holds the turns that the chat itself started. Other messages land in the same chat too:

- the result of a [scheduled task](schedules.md), which runs in its own session (this includes a script schedule with `-on-output deliver` or `agent`);
- a report or `message_home` post from another agent, when this chat is that agent's [home chat](multi-agent.md#teams-and-home-chats);
- the result of a background task, a board update, or something sent with `hk send` or the MCP `habakkuk_send` tool.

Habakkuk keeps a note of each of these in `state.db`, for the chat it went to. That means the same channel account, chat and thread. When you send your next message, the agent that answers the chat sees the notes it hasn't seen yet, placed before your words. It looks like this:

```text
[Earlier in this chat, at 18:44, your scheduled task "Inbox check" sent: "3 new emails, 1 needs an answer today."]
[Earlier in this chat, at 18:05, agent engineering sent: "Tests pass. Merge it?"]
how did you do that?
```

That way, a reply such as "yes" or "how did you do that?" is read against the message you're answering. This works even when that message came from the agent's own scheduled run.

- **Once each.** A note is shown to one turn and then marked as seen. If that turn crashes before its message is saved, the retry shows the same notes. So no note is lost and no note appears twice.
- **Never mid-turn.** Notes arrive with your next message. Habakkuk never inserts them into a turn that's already running.
- **Bounded.** Each note is cut to 1,200 characters, and one turn takes at most 6,000 characters of notes. Habakkuk keeps the newest notes and adds a line that says how many older ones it left out.
- **Not the chat's own replies.** Anything the chat's own session sent is already in its history, so it leaves no note.

Notes are pruned together with the rest of `state.db`, and they follow transcript retention.

## Steering a turn

Send these commands in the chat to steer the agent:

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

For every chat command, see the [CLI reference](../reference/cli.md#chat-commands).

## Standing goals

A standing goal keeps the agent working turn after turn. Send `/goal [-turns N] <text>` to set one. The agent keeps going until it replies that the goal is done or blocked. It gets 10 turns by default, and at most 100. `/goal` on its own shows the goal and the turns used. `/goal off` stops the goal after the current step. If the agent needs you, the goal pauses. Answer it, then send `/goal` again to resume.

## Titles

A session's title starts out as its first message. After the first turn, `title_model` writes a short title in the background. By default that's the agent's `fast_model`. Send `/title [text]` to show the title or set it.

## Compaction

When a session gets long, Habakkuk compacts it. Nothing is rewritten. Older messages are summarized into a record, and the model is sent that summary plus the recent turns. The full transcript is still there for `hk show`.

- Compaction happens on its own once a prompt reaches the agent's `compact_at`. The default is 600000 tokens on `anthropic` providers and 150000 otherwise. `-1` turns it off.
- To compact right away, send `/compact` or run `hk sessions compact ID`.

## Fork and rewind

- `/fork` copies the session into a new one, so you can try another direction.
- `/rewind [N]` does the same, but leaves out your last N messages (default 1).

In both cases, the original session stays as it was.

## Export and search

- `/export` sends you the transcript as a Markdown file.
- `hk sessions export ID -format md|json|html [-o FILE]` writes the transcript from the terminal.
- `hk sessions search [-agent A] QUERY` searches every session.
- `hk sessions archive|unarchive|rm ID...` manages sessions, and `hk sessions archived` lists the archived ones.
- An agent can search its own past sessions with the `session_search` tool.

```bash
hk sessions                     # recent sessions
hk show <session>               # print a transcript
```

## Sub-agents

Send `/subagents` to list the sub-agent tasks running for this conversation. `/subagents stop ID` cancels one. See [Delegation](multi-agent.md#delegation).

For the details, see [Architecture: compaction](../reference/architecture.md#compaction) and [session tools](../reference/architecture.md#session-tools).
