---
title: Memory
description: "Long-term memory as Markdown notes, the user profile, untrusted notes, semantic search, background review, and a history of every change you can undo."
order: 3
---

Agents get the `memory` toolset by default. A config that lists `toolsets` itself gets exactly that list (for example `toolsets: [core]` turns memory off), and the prompt only mentions memory when the agent has the toolset.

## Notes

Memory is one Markdown file per note in `agents/<name>/memory/`. Habakkuk saves durable facts, preferences and decisions there with `memory_save`, and finds them with `memory_search` (SQLite FTS5 with bm25 ranking, plus semantic search when embeddings are configured). A note looks like this:

```markdown
---
title: Coffee preference
tags: [food, preferences]
created: "2026-11-06T09:12:00Z"
updated: "2026-11-06T09:12:00Z"
---

Flat white with oat milk, no sugar.
```

The frontmatter also records where the note came from (`source`, `session`, `trust`).

- **Edit by hand** at any time. The index picks the change up on the next search.
- **Forgotten notes** move to `memory/.trash/`.
- **No secrets.** Text that looks like a key, token, password or card number is refused.
- **In the prompt.** Each new session's prompt lists the most recent memory titles, as a snapshot taken when the session starts.

## The user profile

`agents/<name>/USER.md` is a short file of `## Section` blocks with durable facts about you: your name, where you live, how you like to be helped. Every new session starts with it in the prompt. The agent edits one section at a time with `user_profile_update`, and you can edit it by hand.

## Untrusted notes

A note saved in a turn that read outside content (web pages, search results, MCP and other network tools, an email inbox channel) is marked `trust: untrusted` and shown to the model with a warning. Text planted in a page cannot quietly become something the agent believes. Such turns cannot change `USER.md` at all. `habakkuk memory trust <slug>` vouches for a note.

## In a chat and the terminal

In a chat, `/remember <text>` saves your words as a note, and `/forget <what or slug>` removes one (several matches are listed rather than guessed).

```bash
habakkuk memory                       # an agent's memories, newest first
habakkuk memory search coffee         # search them
habakkuk memory show <slug>           # one note, with where it came from
habakkuk memory forget <slug>         # move a note to the memory trash
habakkuk memory trust <slug> [yes|no] # vouch for a note written from outside content
habakkuk memory profile               # the agent's profile of you (USER.md)
habakkuk memory log                   # what the background review changed
```

Add `-agent NAME` for an agent other than the default.

## Semantic search, review and the nightly pass

Optional extras, in a `memory:` section:

```yaml
memory:
  embeddings: {provider: openai, model: text-embedding-3-small}   # or a provider with preset: ollama, or gemini
  review: true            # turn idle conversations into notes and profile updates
  review_model: anthropic/claude-sonnet-5-5   # default: the agent's fast_model, else its last fallback
  review_idle: 15m
  nightly: "03:30"        # merge near-duplicate notes, archive stale ones; or off
  stale_days: 365         # -1 never archives; notes tagged pinned or keep are never archived
```

- **Embeddings.** Notes are embedded when saved (cached by content, so unchanged notes are never sent again), and search blends keyword and meaning matches. If the embedding service fails, search carries on with keywords.
- **Review.** With `review: true`, `habakkuk serve` reviews each conversation once it has been quiet for `review_idle`, saving what is worth keeping with `source: review`. It writes one line per review to `memory/.review.log` (`habakkuk memory log`).
- **Nightly pass.** It merges near-duplicate notes and archives notes neither updated nor read for `stale_days`.

[Architecture: memory](../reference/architecture.md#memory) describes the index, ranking, provenance and crash safety.

## Memory history and undo

Nothing an agent remembers changes silently. Every change to a memory note, `USER.md`, `IDENTITY.md` or one of the agent's own skills is recorded in `$HABAKKUK_HOME/history.db`, with:

- the text before and after;
- who made it: the agent through a tool such as `memory_save` or `set_identity`; you through a chat command, the CLI or the dashboard; the background review; the skills curator; or a hand edit;
- the session, whether outside content had entered that turn, and when.

Edits you make to the files yourself are picked up too: when the index next looks at a note, and once a minute for everything else while `habakkuk serve` runs.

```bash
habakkuk memory history [-agent A] [-file NAME] [-since 7d]   # newest first, with change ids
habakkuk memory diff <id>                                     # one change as a unified diff
habakkuk memory restore <id> [-after]                         # put back the text before it (or after it, to redo)
habakkuk memory pin <slug> | skill:<name> | profile            # background jobs leave it alone
habakkuk memory unpin <slug> | skill:<name> | profile
habakkuk memory pins
```

- **Undo is a change too.** Restoring records a new change that names the one it went back to, so an undo can itself be undone. Restoring a version in which the file did not exist moves it to a `.trash/` folder beside it. For a skill the history holds `SKILL.md`; a skill the curator merged away or archived gets its supporting files back with `habakkuk skills restore NAME`.
- **In chat**, `/memory` lists the last 10 changes for the conversation's agent with their short ids, and `/undo-memory <id>` undoes one. The agent has a read-only `memory_history` tool, so you can ask it "what did you change in my profile?".
- **Pinned** notes, `USER.md` and skills are never rewritten, merged away or archived by the background review, the nightly pass or the skills curator. You and the agent can still edit them.
- **Crash-safe.** A change is first written to the history as pending, then the file is replaced atomically, then the change is marked done. After a crash the pending change is kept if the file holds the new text and dropped if it does not, so the history never shows a change that did not happen and never misses one that did.
- **Retention.** Changes are kept for `memory.history_days` (default 90, `-1` keeps everything) and pruned by the same maintenance pass as `retention.days`. The newest version of every file that still exists is always kept. Content is stored once per distinct text and compressed.
- **API.** The dashboard API has the same: `GET /api/memory-history/{agent}`, `GET /api/memory-history/{agent}/{id}` and `POST /api/memory-history/{agent}/{id}/restore` ([HTTP API](../reference/api.md#memory-history)).

```yaml
memory:
  history_days: 90        # -1 keeps all history
```
