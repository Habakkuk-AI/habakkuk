---
title: Memory and skills
description: Long-term memory as Markdown notes, the user profile, untrusted notes, semantic search and background review; skills as SKILL.md folders.
order: 6
---

Agents get the `memory` and `skills` toolsets by default, along with `core`, `schedule`, `delegate` and `board`. A config that lists `toolsets` itself gets exactly that list (for example `toolsets: [core]` turns memory and skills off), and the prompt only mentions memory or skills when the agent has the matching toolset.

## Memory

- **Memory** is one Markdown file per note in `agents/<name>/memory/`, with `title`, `tags`, `created` and `updated` in its frontmatter, plus where it came from (`source`, `session`, `trust`). Habakkuk saves durable facts, preferences and decisions there with `memory_save` and finds them with `memory_search` (SQLite FTS5, bm25 ranking, and semantic search when embeddings are configured). Edit or add notes by hand at any time; the index picks the change up on the next search. Forgotten notes move to `memory/.trash/`. Text that looks like a key, token, password or card number is refused.
- **The user profile** is `agents/<name>/USER.md`: a short file of `## Section` blocks with durable facts about you (name, where you live, how you like to be helped). Every new session starts with it in the prompt. The agent edits one section at a time with `user_profile_update`; you can edit it by hand.
- **In chat**, `/remember <text>` saves your words as a note and `/forget <what or slug>` removes one (several matches are listed rather than guessed).
- **Untrusted notes.** A note saved in a turn that read outside content (web pages, search results, MCP and other network tools, an email inbox channel) is marked `trust: untrusted` and shown to the model with a warning, so text planted in a page cannot quietly become something the agent believes. Such turns cannot change USER.md at all. `habakkuk memory trust <slug>` vouches for a note.

A note looks like this:

```markdown
---
title: Coffee preference
tags: [food, preferences]
created: "2026-11-06T09:12:00Z"
updated: "2026-11-06T09:12:00Z"
---

Flat white with oat milk, no sugar.
```

From the terminal:

```bash
habakkuk memory                       # an agent's memories, newest first
habakkuk memory search coffee         # search them
habakkuk memory show <slug>           # one note, with where it came from
habakkuk memory forget <slug>         # move a note to the memory trash
habakkuk memory trust <slug> [yes|no] # vouch for a note written from outside content
habakkuk memory profile               # the agent's profile of you (USER.md)
habakkuk memory log                   # what the background review changed
```

Each new session's prompt lists the most recent memory titles and every skill's description, as a snapshot taken when the session starts.

### Semantic search, review and the nightly pass

Optional extras, in a `memory:` section:

```yaml
memory:
  embeddings: {provider: openai, model: text-embedding-3-small}   # or a provider with preset: ollama, or gemini
  review: true            # turn idle conversations into notes and profile updates
  review_model: anthropic/claude-sonnet-5-5   # default: each agent's last fallback model
  review_idle: 15m
  nightly: "03:30"        # merge near-duplicate notes, archive stale ones; or off
  stale_days: 365         # -1 never archives; notes tagged pinned or keep are never archived
```

With embeddings, notes are embedded when saved (cached by content, so unchanged notes are never sent again) and search blends keyword and meaning matches; if the embedding service fails, search carries on with keywords. With `review: true`, `habakkuk serve` reviews each conversation once it has been quiet for `review_idle`, saving what is worth keeping with `source: review`, and writes one line per review to `memory/.review.log` (`habakkuk memory log`). The nightly pass merges near-duplicate notes and archives notes neither updated nor read for `stale_days`.

[Architecture: memory](../reference/architecture.md#memory) describes the index, ranking, provenance and crash safety.


## Memory history and undo

Nothing an agent remembers changes silently. Every change to a memory note, `USER.md`, `IDENTITY.md` or one of the agent's own skills is recorded in `$HABAKKUK_HOME/history.db` with the text before and after, who made it (the agent through a tool such as `memory_save` or `set_identity`, you through a chat command, the CLI or the dashboard, the background review, the skills curator, or a hand edit), the session, whether external content had entered that turn, and when. Edits you make to the files yourself are picked up too: when the index next looks at a note, and once a minute for everything else while `habakkuk serve` runs.

```
habakkuk memory history [-agent A] [-file NAME] [-since 7d]   # newest first, with change ids
habakkuk memory diff <id>                                     # one change as a unified diff
habakkuk memory restore <id> [-after]                         # put back the text before it (or after it, to redo)
habakkuk memory pin <slug> | skill:<name> | profile            # background jobs leave it alone
habakkuk memory unpin <slug> | skill:<name> | profile
habakkuk memory pins
```

- **Undo is a change too.** Restoring records a new change that names the one it went back to, so an undo can itself be undone. Restoring a version in which the file did not exist moves it to a `.trash/` folder beside it. For a skill the history holds `SKILL.md`; a skill the curator merged away or archived gets its supporting files back with `habakkuk skills restore NAME`.
- **In chat**, `/memory` lists the last 10 changes for the conversation's agent with their short ids, and `/undo-memory <id>` undoes one. The agent has a read-only `memory_history` tool, so you can ask it "what did you change in my profile?".
- **Pinned** notes, `USER.md` and skills are never rewritten, merged away or archived by the background review, the nightly pass or the skills curator; you and the agent can still edit them.
- **Crash-safe.** A change is first written to the history as pending, then the file is replaced atomically, then the change is marked done. After a crash the pending change is kept if the file holds the new text and dropped if it does not, so the history never shows a change that did not happen and never misses one that did.
- **Retention.** Changes are kept for `memory.history_days` (default 90, `-1` keeps everything) and pruned by the same maintenance pass as `retention.days`. The newest version of every file that still exists is always kept. Content is stored once per distinct text and compressed.
- The dashboard API has the same: `GET /api/memory-history/{agent}`, `GET /api/memory-history/{agent}/{id}` and `POST /api/memory-history/{agent}/{id}/restore` ([docs/API.md](docs/API.md)).

```yaml
memory:
  history_days: 90        # -1 keeps all history
```

## Skills

**Skills** are folders with a `SKILL.md` (frontmatter `name` and `description`, then instructions) and optional supporting files. Put shared skills in `$HABAKKUK_HOME/skills/<name>/` and an agent's own in `agents/<agent>/skills/<name>/`, which wins on a name clash. Habakkuk loads a skill with `skill_view` before a task it covers and can write its own with `skill_save` (the previous version is kept as `SKILL.md.bak`).

```markdown
---
name: weekly-report
description: Write the weekly report from the board and the week's sessions.
---

1. ...
```

The folder name is the skill name and must be lower-case letters, digits and hyphens (up to 64 characters).

- **In a chat**, `/skill NAME [request]` runs one explicitly, `/skills` lists them, and `/learn <topic, URL or path>` has the agent study a source and write a new skill. `habakkuk run -skill NAME [request]` does the same from the terminal.
- **Installing.** Install skills and plugins from GitHub, URLs, local paths or registries with `habakkuk skills install` and `habakkuk plugins install`; installed skills are untrusted until `habakkuk skills approve`. See [Plugins, outside skills and the MCP catalog](plugins.md).
- **Curator.** With `skills.curate: daily | weekly | monthly`, `habakkuk serve` merges near-duplicate skills the agents wrote and archives ones not loaded for `skills.archive_after_days` (default 90), with backups; `habakkuk skills restore NAME` undoes a change and `habakkuk skills curate [-dry-run]` runs it by hand.

```yaml
skills:
  registries:
    - {name: team, url: "https://skills.example.com/index.json"}
  curate: weekly          # off | daily | weekly | monthly
  archive_after_days: 90
```

[Architecture: skills](../reference/architecture.md#skills) has the details.
