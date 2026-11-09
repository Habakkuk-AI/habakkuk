---
title: Skills
description: "Skills are SKILL.md folders an agent loads before a task it covers: where they live, how to run and write them, installing outside skills, and the curator."
order: 4
---

A skill is a folder with a `SKILL.md` (frontmatter `name` and `description`, then instructions) and optional supporting files. Agents get the `skills` toolset by default.

```markdown
---
name: weekly-report
description: Write the weekly report from the board and the week's sessions.
---

1. ...
```

The folder name is the skill name and must be lower-case letters, digits and hyphens (up to 64 characters).

## Where skills live

| Folder | Used by |
|---|---|
| `$HABAKKUK_HOME/skills/<name>/` | Every agent (shared skills, including installed ones) |
| `$HABAKKUK_HOME/agents/<agent>/skills/<name>/` | That agent only; wins on a name clash |
| An enabled plugin's skills folder | Every agent, below the two above |

Each new session's prompt lists every skill's description, as a snapshot taken when the session starts. Habakkuk loads a skill with `skill_view` before a task it covers, reads its supporting files with `skill_file`, and can write its own with `skill_save` (the previous version is kept as `SKILL.md.bak`).

## Using skills

- **In a chat**, `/skills` lists them, `/skill NAME [request]` runs one explicitly, and `/learn <topic, URL or path>` has the agent study a source and write a new skill.
- **From the terminal**, `habakkuk run -skill NAME [request]` does the same, and `habakkuk skills [-agent A]` lists the skills an agent can use, where each comes from, and whether it is approved.

## Installing skills from elsewhere

```bash
habakkuk skills install github:owner/repo/skill     # also URLs, local paths and registry names
habakkuk skills approve NAME                         # after reading it
```

Installed skills are untrusted until `habakkuk skills approve`. A skill that ships scripts stays disabled until then; one without scripts loads with a caution in front of it. Every install is pinned, hashed and scanned. See [Plugins and outside skills](../tools/plugins.md).

## The curator

With `skills.curate` set, `habakkuk serve` merges near-duplicate skills the agents wrote and archives ones not loaded for `skills.archive_after_days` (default 90), with backups.

```yaml
skills:
  registries:
    - {name: team, url: "https://skills.example.com/index.json"}
  curate: weekly          # off | daily | weekly | monthly
  archive_after_days: 90
```

```bash
habakkuk skills curate [-dry-run] [-agent A]   # run it by hand
habakkuk skills restore NAME                    # undo a merge or archive
habakkuk memory pin skill:NAME                  # keep the curator away from one skill
```

Every change to an agent's own skills is in the [memory history](memory.md#memory-history-and-undo).

[Architecture: skills](../reference/architecture.md#skills) has the details.
