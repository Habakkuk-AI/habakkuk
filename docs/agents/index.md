---
title: Agents
description: "What an agent is in Habakkuk, and where to read about identity, teams, memory, skills, schedules, the board, approvals, sessions and retros."
order: 3
---

An agent is a configured assistant: its models, permission mode, workspace, toolsets, and the files that shape how it talks. One Habakkuk can run several agents, each with its own memory and skills, and they can hand each other work.

```yaml
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
    permission: workspace
    workspace: ~/habakkuk-workspace
```

Every message an agent handles is a **turn**: a durable job that runs the model, calls tools, and ends with a reply. Turns belong to a **session**, the conversation's history.

## In this section

<!-- cards -->
- [Agents and identity](agents.md): define an agent, give it a personality and instructions, and how the first chat lets you name it.
- [Multi-agent teams](multi-agent.md): route conversations to agents, delegate to sub-agents, and give each agent a home chat.
- [Memory](memory.md): Markdown notes, the user profile, semantic search, background review, and undo for every change.
- [Skills](skills.md): `SKILL.md` folders an agent loads before a task, and writes for itself.
- [Schedules](schedules.md): prompts and scripts on a cron, at most once per slot.
- [Task board](board.md): cards one agent hands another, that outlive the conversation.
- [Permissions and approvals](approvals.md): what an agent may do on its own, the reviewer model, hooks and safety guards.
- [Sessions and chat commands](sessions.md): titles, compaction, fork and rewind, standing goals, and the `/` commands.
- [Retros](retros.md): an agent looks back over its own work, finds causes in the evidence, and proposes changes you approve.

## Defaults worth knowing

- An agent without `toolsets` gets `core`, `memory`, `skills`, `schedule`, `delegate` and `board`. A config that lists `toolsets` gets exactly that list.
- The default permission mode is `workspace`: reads and writes inside the workspace run on their own, commands ask first.
- Messages from chat apps need approval for commands even in `full` mode, unless you opt in with `trust: full`. See [Security](../deploy/security.md).
