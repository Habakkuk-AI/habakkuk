---
title: Skills
description: "A skill is a folder with a SKILL.md that an agent loads before a task: the skills that come with Habakkuk, where skills live, how to run, write, install and turn them off, and what the curator does."
order: 4
---

A skill is a set of instructions that an agent loads before a task the skill covers. This page lists the skills that come with Habakkuk, and shows where skills live, how to use, install and turn them off, and what the curator does. On disk, a skill is a folder with a `SKILL.md` and optional supporting files. The file starts with frontmatter that has a `name` and a `description`, and the instructions come after it. Agents get the `skills` toolset by default.

```markdown
---
name: weekly-report
description: Write the weekly report from the board and the week's sessions.
---

1. ...
```

The folder name is the skill's name. It must be made of lower-case letters, digits and hyphens, and it can be up to 64 characters long.

## Skills that come with Habakkuk

Habakkuk ships with a set of skills inside the program, so a new install can do useful work before you add anything. `hk skills` lists them, marked `bundled`.

| For | Skills |
|---|---|
| Your day | `daily-brief`, `inbox-triage`, `email-draft`, `follow-ups`, `weekly-review` |
| Meetings | `meeting-prep`, `meeting-notes` |
| Finding out | `research-report`, `fact-check`, `summarize`, `news-digest`, `web-monitor` |
| Documents and decisions | `doc-to-actions`, `decision-memo`, `project-plan`, `brainstorm` |
| Writing | `plain-writing`, `proofread`, `social-post` |
| Numbers and files | `data-analysis`, `expense-log`, `file-organizer` |
| Learning and travel | `study-guide`, `trip-planner` |
| Code | `code-review`, `debugging`, `write-tests`, `pr-description`, `release-notes`, `incident-report`, `github` |
| Connected services | `google-workspace` (through an MCP server you connect) |
| Skills themselves | `write-a-skill` |

Some things to know about them:

- They live in the program, not on disk, so an update brings the newest versions and nothing can edit them by accident.
- Every other skill of the same name wins over a bundled one. To change one, save your own copy under the same name: ask the agent to, or put a folder in `$HABAKKUK_HOME/skills/`.
- You can turn any of them off, or all of them. See [Turning a skill off](#turning-a-skill-off).
- `github` is offered only on a machine that has the `gh` program. See [What a skill requires](#what-a-skill-requires).

## Where skills live

| Place | Used by |
|---|---|
| `$HABAKKUK_HOME/agents/<agent>/skills/<name>/` | That agent only; wins on a name clash |
| `$HABAKKUK_HOME/skills/<name>/` | Every agent (shared skills, including installed ones) |
| An enabled plugin's skills folder | Every agent, below the two above |
| Inside the program (bundled) | Every agent, below everything else |

The prompt of each new session lists the description of every skill. The list is a snapshot taken when the session starts. Before a task that a skill covers, Habakkuk loads the skill with `skill_view`. It reads the skill's supporting files with `skill_file`. It can also write skills of its own with `skill_save`, which keeps the previous version as `SKILL.md.bak`.

## Using skills

- **In a chat**, every skill is a command of its own: `/daily-brief`, or `/meeting-notes` followed by what to apply it to. `/skills` lists them. `/skill NAME [request]` does the same as `/NAME [request]`, and `/learn <topic, URL or path>` has the agent study a source and write a new skill.
- **From the terminal**, `hk run -skill NAME [request]` runs one. `hk skills [-agent A]` lists the skills an agent can use and where each one comes from, then the ones it isn't offered and why.

The skill commands work on every chat app, in `hk chat` and in the terminal UI. Some details:

- Habakkuk's own commands and a plugin's commands come first. A skill named `usage`, `stop` or `remember` never becomes that command, in a chat app, in `hk chat` or in the terminal UI. Run it with `/skill usage`. `hk skills`, `/skills`, an install and the agent's `skill_save` all tell you when a skill's name is taken this way.
- An underscore works in place of a hyphen (`/daily_brief`), for chat apps whose command names can't hold a hyphen.
- The command runs the skill for the agent the chat goes to, so two agents can each have their own `/weekly-report`.

## Turning a skill off

You can turn a skill off without removing it. A skill that is off isn't listed in the prompt or by `/skills`, can't be loaded, and has no command. Its files stay where they are.

```bash
hk skills disable news-digest      # off for every agent, from their next session
hk skills enable news-digest       # back on
```

The name is all that counts, so this works for a bundled skill, a shared one, a plugin's and an agent's own. `hk serve` doesn't need a restart.

Turning a skill on never approves one. A skill that's waiting for you stays out until you approve that skill itself: one you installed that ships scripts (`hk skills approve NAME`), one `hk migrate` brought in from a project (`hk skills approve -agent AGENT NAME`), or one an agent saved after reading outside content (`hk skills approve AGENT/NAME`). While it waits, the agent uses whatever else has that name, such as the bundled skill.

You can keep the list in `config.yaml` instead, and turn off every bundled skill at once:

```yaml
skills:
  bundled: true                   # false turns off every skill that ships with Habakkuk
  disabled: [news-digest, social-post]
```

A name in `skills.disabled` stays off until you take it out of the file. `hk skills enable` tells you so.

## What a skill requires

A skill can say what it needs from the machine Habakkuk runs on, in a `requires` block in its frontmatter:

```markdown
---
name: github
description: Work with GitHub issues, pull requests, checks and releases through gh.
requires:
  bins: [gh]              # programs that must be on PATH
  env: [GITHUB_TOKEN]     # variables that must be set, in the environment or in .env
  os: [darwin, linux]     # operating systems it works on: darwin, linux, windows
---
```

Every key is optional. A skill whose requirements aren't met is treated like one that is off: agents aren't offered it, so they never start a job they can't finish. `hk skills` lists it under "Not offered" with what is missing, for example "needs the program gh on PATH". Install the program or set the variable, and the skill appears in the next session.

When an agent rewrites one of its own skills, the `requires` block is kept.

## Installing skills from elsewhere

Install a skill, read it, then approve it:

```bash
hk skills search invoices                           # look in the public directory
hk skills install invoice-chaser                    # a name from it
hk skills install github:owner/repo/skill           # also URLs and local paths
hk skills approve NAME                               # after reading it
```

Names come from the public directory at [habakkuk.ai/skills](https://www.habakkuk.ai/skills), which Habakkuk uses as its default registry. It is contacted only when you run `hk skills` or `hk plugins` with `search`, `install` or `update`. To stop using it, set `skills.default_registry: "off"`. See [Registries](../tools/plugins.md#registries).

An installed skill is untrusted until you run `hk skills approve`. Until then, a skill that ships scripts stays disabled, and one without scripts loads with a caution in front of it. A skill `hk migrate` brought in from a project folder stays disabled until you run `hk skills approve -agent AGENT NAME`, scripts or not. Every install is pinned, hashed and scanned. See [Plugins and outside skills](../tools/plugins.md).

## The curator

The curator tidies up the skills that agents write. When `skills.curate` is set, `hk serve` merges near-duplicate skills the agents wrote. It also archives skills that haven't been loaded for `skills.archive_after_days` (default 90). It keeps backups.

```yaml
skills:
  curate: weekly          # off | daily | weekly | monthly
  archive_after_days: 90
```

The curator only touches skills the agents wrote. It leaves bundled, installed and plugin skills alone.

```bash
hk skills curate [-dry-run] [-agent A]         # run it by hand
hk skills restore NAME                          # undo a merge or archive
hk memory pin skill:NAME                        # keep the curator away from one skill
```

Every change to an agent's own skills is recorded in the [memory history](memory.md#memory-history-and-undo).

For the details, see [Architecture: skills](../reference/architecture.md#skills).
