---
title: Agents
description: "What an agent is in Habakkuk, and which page covers identity, teams, memory, skills, schedules, the board, approvals, sessions and retros."
order: 3
---

This section covers how you set up agents and what they can do. An agent is an assistant you configure. You choose its models, permission mode, workspace and toolsets, and you write the files that shape how it talks. One Habakkuk can run several agents. Each has its own memory and skills, and they can hand each other work.

```yaml
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
    permission: workspace
    workspace: ~/habakkuk-workspace
```

Every message an agent handles is a **turn**. A turn is a durable job: it runs the model, calls tools and ends with a reply. Turns belong to a **session**, which is the conversation's history.

## In this section

<!-- cards -->
- [Agents and identity](agents.md): define an agent, give it a personality and instructions, and see how the first chat lets you name it.
- [Multi-agent teams](multi-agent.md): route conversations to agents, delegate to sub-agents, and give each agent a home chat.
- [Memory](memory.md): what an agent remembers in Markdown notes and the user profile, with semantic search, background review, and undo for every change.
- [Skills](skills.md): folders with a `SKILL.md` that an agent loads before a task, and can write for itself.
- [Schedules](schedules.md): run prompts and scripts on a cron, at most once per slot.
- [Task board](board.md): cards that one agent hands to another, and that outlive the conversation.
- [Permissions and approvals](approvals.md): what an agent may do on its own, plus the reviewer model, hooks and safety guards.
- [Sessions and chat commands](sessions.md): how sessions work (titles, compaction, fork and rewind, standing goals) and the `/` commands.
- [Retros](retros.md): an agent looks back over its own work, finds causes in the evidence, and proposes changes for you to approve.

## Defaults worth knowing

- If you don't set `toolsets`, an agent gets `core`, `memory`, `skills`, `schedule`, `delegate` and `board`. If your config does list `toolsets`, the agent gets exactly that list.
- The default permission mode is `workspace`. Reads and writes inside the workspace run on their own, and commands ask first.
- A message from a chat app needs approval before it can run commands, even in `full` mode. That changes only if you opt in with `trust: full`. See [Security](../deploy/security.md).
