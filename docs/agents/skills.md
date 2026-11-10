---
title: Skills
description: "A skill is a folder with a SKILL.md that an agent loads before a task: where skills live, how to run, write and install them, and what the curator does."
order: 4
---

A skill is a set of instructions that an agent loads before a task the skill covers. This page shows where skills live, how to use and install them, and what the curator does. On disk, a skill is a folder with a `SKILL.md` and optional supporting files. The file starts with frontmatter that has a `name` and a `description`, and the instructions come after it. Agents get the `skills` toolset by default.

```markdown
---
name: weekly-report
description: Write the weekly report from the board and the week's sessions.
---

1. ...
```

The folder name is the skill's name. It must be made of lower-case letters, digits and hyphens, and it can be up to 64 characters long.

## Where skills live

| Folder | Used by |
|---|---|
| `$HABAKKUK_HOME/skills/<name>/` | Every agent (shared skills, including installed ones) |
| `$HABAKKUK_HOME/agents/<agent>/skills/<name>/` | That agent only; wins on a name clash |
| An enabled plugin's skills folder | Every agent, below the two above |

The prompt of each new session lists the description of every skill. The list is a snapshot taken when the session starts. Before a task that a skill covers, Habakkuk loads the skill with `skill_view`. It reads the skill's supporting files with `skill_file`. It can also write skills of its own with `skill_save`, which keeps the previous version as `SKILL.md.bak`.

## Using skills

- **In a chat**, `/skills` lists them and `/skill NAME [request]` runs one explicitly. `/learn <topic, URL or path>` has the agent study a source and write a new skill.
- **From the terminal**, `hk run -skill NAME [request]` does the same. `hk skills [-agent A]` lists the skills an agent can use, where each one comes from, and whether it's approved.

## Installing skills from elsewhere

Install a skill, read it, then approve it:

```bash
hk skills install github:owner/repo/skill           # also URLs, local paths and registry names
hk skills approve NAME                               # after reading it
```

An installed skill is untrusted until you run `hk skills approve`. Until then, a skill that ships scripts stays disabled, and one without scripts loads with a caution in front of it. A skill `hk migrate` brought in from a project folder stays disabled until you run `hk skills approve -agent AGENT NAME`, scripts or not. Every install is pinned, hashed and scanned. See [Plugins and outside skills](../tools/plugins.md).

## The curator

The curator tidies up the skills that agents write. When `skills.curate` is set, `hk serve` merges near-duplicate skills the agents wrote. It also archives skills that haven't been loaded for `skills.archive_after_days` (default 90). It keeps backups.

```yaml
skills:
  registries:
    - {name: team, url: "https://skills.example.com/index.json"}
  curate: weekly          # off | daily | weekly | monthly
  archive_after_days: 90
```

```bash
hk skills curate [-dry-run] [-agent A]         # run it by hand
hk skills restore NAME                          # undo a merge or archive
hk memory pin skill:NAME                        # keep the curator away from one skill
```

Every change to an agent's own skills is recorded in the [memory history](memory.md#memory-history-and-undo).

For the details, see [Architecture: skills](../reference/architecture.md#skills).
