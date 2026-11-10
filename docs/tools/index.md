---
title: Tools
description: "Which toolsets an agent can use, which one fits the job you have in mind, and the page that explains each of them."
order: 5
---

Start here to pick the right tools for an agent. Each one has a page of its own, linked below.

Tools come in groups called toolsets. An agent gets the toolsets you list under `toolsets`. If you leave the setting out, it gets `core`, `memory`, `skills`, `schedule`, `delegate` and `board`. Your list replaces that default, so include any of the defaults you still want:

```yaml
agents:
  habakkuk:
    toolsets: [core, memory, skills, schedule, delegate, board, browser]
```

The agent's [permission mode and approvals](../agents/approvals.md) decide what a tool can do without asking you first.

## If you need X, use Y

| You need the agent to | Use | Page |
|---|---|---|
| Read, write and patch files, run commands | `core` (default) | [Files and shell](files-and-shell.md) |
| Run commands in a container or on another host | `sandbox: docker` or `ssh` | [Files and shell](files-and-shell.md#sandboxes) |
| Undo what a turn changed in the workspace | checkpoints (on by default), `/undo` | [Files and shell](files-and-shell.md#checkpoints) |
| Browse the web, fill in forms, use sites you are signed in to | `browser` | [Browser](browser.md) |
| Drive desktop apps with the mouse and keyboard | `computer` | [Computer use](computer-use.md) |
| Search the web | hosted search, or `search:` | [Media and web search](media.md#web-search) |
| Read images, PDFs and Office files; make images and video | `core` with `images:` or `video:` | [Media and web search](media.md) |
| Talk out loud, or answer voice notes with voice | `realtime:`, `hk talk`, `tts:` | [Voice](voice.md) |
| Sit in a video call and take notes | `meeting` | [Meetings](meetings.md) |
| Use GitHub, Linear, Notion, a database and hundreds more | `mcp:<server>` | [MCP](mcp.md) |
| Install someone's packaged skills, tools and commands | `hk plugins install` | [Plugins](plugins.md) |
| Control Home Assistant, talk to other agents, lend it a phone's camera | `homeassistant`, `a2a`, device nodes | [Integrations](integrations.md) |
| Remember things and follow skills | `memory`, `skills` (default) | [Memory](../agents/memory.md), [Skills](../agents/skills.md) |
| Schedule work and hand it to other agents | `schedule`, `delegate`, `board` (default) | [Schedules](../agents/schedules.md), [Multi-agent](../agents/multi-agent.md) |

## Toolsets

| Toolset | Tools |
|---|---|
| `core` | `read_file`, `write_file`, `edit_file`, `list_dir`, `exec`, `apply_patch`, `search_files`, `fetch_url`, background processes, `todo`, `clarify`, `view_image`, `read_document`; with channels `send_file` and `poll`; `image_generate`, `video_generate`, `speak`, `nodes_list` and `node_invoke` when configured |
| `memory` | `memory_save`, `memory_search`, `memory_read`, `memory_forget`, `user_profile_read`, `user_profile_update`, `session_search`, `memory_history` |
| `skills` | `skill_view`, `skill_file`, `skill_save` |
| `schedule` | `schedule_create` and friends |
| `delegate` | `delegate` |
| `board` | `board_*` |
| `browser` | `browser_*` |
| `computer` | `computer_*` |
| `meeting` | `meeting_join`, `meeting_leave`, `meeting_status`, `meeting_transcript` |
| `homeassistant` | `ha_states`, `ha_call_service`, `ha_history` |
| `a2a` | `a2a_discover`, `a2a_send` |
| `mcp:<server>` | One per MCP server, named `mcp__<server>__<tool>` |

If an agent has a [home chat](../agents/multi-agent.md#teams-and-home-chats), it gets `message_home` as well.

## Tool search

Some agents end up with a lot of tools, usually because of MCP servers. When an agent's toolsets hold more tools than `tool_search_over`, it's offered only the `core` tools plus `tool_search` and `tool_describe`. Any tool it finds that way is added for the rest of the session. The default limit is 60, and `-1` turns it off.

You set the limit on the agent:

```yaml
agents:
  habakkuk:
    tool_search_over: 60
```
