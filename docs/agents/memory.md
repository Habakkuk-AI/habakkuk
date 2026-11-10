---
title: Memory
description: "How an agent keeps long-term memory in Markdown notes and a user profile, with untrusted notes, semantic search, background review and undo for every change."
order: 3
---

An agent remembers things between conversations, and you can read, search, correct and undo what it remembers. Agents get the `memory` toolset by default. If your config lists `toolsets` itself, the agent gets exactly that list. For example, `toolsets: [core]` turns memory off. The prompt mentions memory only when the agent has the toolset.

## Notes

Each note is one Markdown file in `agents/<name>/memory/`. Habakkuk saves lasting facts, preferences and decisions there with `memory_save`. It finds them again with `memory_search`, which uses SQLite FTS5 with bm25 ranking, plus semantic search when you've configured embeddings. A note looks like this:

```markdown
---
title: Coffee preference
tags: [food, preferences]
created: "2026-11-06T09:12:00Z"
updated: "2026-11-06T09:12:00Z"
---

Flat white with oat milk, no sugar.
```

The frontmatter also records where the note came from, in `source`, `session` and `trust`.

- **Edit by hand.** You can change a note yourself at any time. The index picks up the change on the next search.
- **Forgotten notes** are moved to `memory/.trash/`.
- **No secrets.** Habakkuk refuses to save text that looks like a key, token, password or card number.
- **In the prompt.** The prompt of each new session lists the most recent memory titles. The list is a snapshot taken when the session starts.

## The user profile

The user profile is `agents/<name>/USER.md`. It's a short file of `## Section` blocks that hold lasting facts about you: your name, where you live, how you like to be helped. Every new session starts with the profile in its prompt. The agent edits one section at a time with `user_profile_update`, and you can edit the file by hand.

## Untrusted notes

Outside content means web pages, search results, MCP and other network tools, and an email inbox channel. A note saved in a turn that has read any of it is marked `trust: untrusted`, and the model sees it with a warning. This stops text planted in a page from quietly becoming something the agent believes. Turns like that can't change `USER.md` at all. To vouch for a note, run `habakkuk memory trust <slug>`.

## In a chat and the terminal

In a chat, send `/remember <text>` to save your words as a note. Send `/forget <what or slug>` to remove one. If several notes match, Habakkuk lists them instead of guessing.

The terminal has these commands:

```bash
habakkuk memory                       # an agent's memories, newest first
habakkuk memory search coffee         # search them
habakkuk memory show <slug>           # one note, with where it came from
habakkuk memory forget <slug>         # move a note to the memory trash
habakkuk memory trust <slug> [yes|no] # vouch for a note written from outside content
habakkuk memory profile               # the agent's profile of you (USER.md)
habakkuk memory log                   # what the background review changed
```

Add `-agent NAME` to work with an agent other than the default.

## Semantic search, review and the nightly pass

These extras are optional. You turn them on in a `memory:` section:

```yaml
memory:
  embeddings: {provider: openai, model: text-embedding-3-small}   # or a provider with preset: ollama, or gemini
  review: true            # turn idle conversations into notes and profile updates
  review_model: anthropic/claude-sonnet-5-5   # default: the agent's fast_model, else its last fallback
  review_idle: 15m
  nightly: "03:30"        # merge near-duplicate notes, archive stale ones; or off
  stale_days: 365         # -1 never archives; notes tagged pinned or keep are never archived
```

- **Embeddings.** Habakkuk embeds each note when it's saved. Embeddings are cached by content, so a note that hasn't changed is never sent again. Search then blends keyword matches with matches on meaning. If the embedding service fails, search carries on with keywords.
- **Review.** With `review: true`, `habakkuk serve` reviews each conversation once it has been quiet for `review_idle`. It saves what's worth keeping, with `source: review`. It also writes one line per review to `memory/.review.log`, which you can read with `habakkuk memory log`.
- **Nightly pass.** This merges notes that are near-duplicates. It also archives notes that haven't been updated or read for `stale_days`.

A review looks at the facts in one conversation. A retro looks wider. In a retro, an agent looks back over a week of its own work (failures, denied approvals, stalled tasks, corrections) and proposes changes to its skills, notes and `SOUL.md`. See [Retros](retros.md).

For the index, ranking, provenance and crash safety, see [Architecture: memory](../reference/architecture.md#memory).

## Memory history and undo

Whenever something an agent remembers changes, Habakkuk keeps a record. It records every change to a memory note, `USER.md`, `IDENTITY.md` or one of the agent's own skills in `$HABAKKUK_HOME/history.db`. Each record has:

- the text before and after;
- who made the change: the agent (through a tool such as `memory_save` or `set_identity`), you (through a chat command, the CLI or the dashboard), the background review, the skills curator, or a hand edit;
- the session, when it happened, and whether outside content had entered that turn.

Habakkuk picks up edits you make to the files yourself too. For a note, that happens when the index next looks at it. For everything else, it happens once a minute while `habakkuk serve` runs.

```bash
habakkuk memory history [-agent A] [-file NAME] [-since 7d]   # newest first, with change ids
habakkuk memory diff <id>                                     # one change as a unified diff
habakkuk memory restore <id> [-after]                         # put back the text before it (or after it, to redo)
habakkuk memory pin <slug> | skill:<name> | profile            # background jobs leave it alone
habakkuk memory unpin <slug> | skill:<name> | profile
habakkuk memory pins
```

- **Undo is a change too.** A restore records a new change that names the one it went back to, so you can undo an undo. If you restore a version in which the file didn't exist, the file moves to a `.trash/` folder beside it. For a skill, the history holds `SKILL.md`. If the curator merged a skill away or archived it, run `habakkuk skills restore NAME` to get its supporting files back.
- **In chat**, `/memory` lists the last 10 changes for the conversation's agent, with their short ids. `/undo-memory <id>` undoes one. The agent also has a read-only `memory_history` tool, so you can ask it "what did you change in my profile?".
- **Pinned** notes, `USER.md` and skills are left alone by the background review, the nightly pass and the skills curator. None of them rewrites, merges away or archives anything that's pinned. You and the agent can still edit it.
- **Crash-safe.** Habakkuk first writes a change to the history as pending. Then it replaces the file atomically. Then it marks the change done. After a crash, it keeps the pending change if the file holds the new text, and drops it if the file doesn't. So the history shows every change that happened and none that didn't.
- **Retention.** Habakkuk keeps changes for `memory.history_days`. The default is 90, and `-1` keeps everything. Older changes are pruned by the same maintenance pass as `retention.days`. The newest version of every file that still exists is always kept. Content is stored once per distinct text, and it's compressed.
- **Retros.** A [retro](retros.md) writes skills, notes and `SOUL.md` through the same history, so you can undo its changes too. Changes it makes on its own (with `retro: {apply: safe}`) are recorded as made by `retro`. Changes you approve are recorded as yours. `SOUL.md` is tracked from the first time Habakkuk changes it.
- **API.** The dashboard API offers the same through `GET /api/memory-history/{agent}`, `GET /api/memory-history/{agent}/{id}` and `POST /api/memory-history/{agent}/{id}/restore`. See [HTTP API](../reference/api.md#memory-history).

```yaml
memory:
  history_days: 90        # -1 keeps all history
```
