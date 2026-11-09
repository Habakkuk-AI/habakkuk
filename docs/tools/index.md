---
title: Tools
description: "The toolsets an agent can use, which one to reach for, and where each is explained: files and shell, browser, computer use, voice, meetings, media, MCP, plugins and integrations."
order: 5
---

Tools come in toolsets, and an agent lists the ones it gets under `toolsets`. Without the setting it gets `core`, `memory`, `skills`, `schedule`, `delegate` and `board`. A list replaces the default, so add the defaults you want to keep:

```yaml
agents:
  habakkuk:
    toolsets: [core, memory, skills, schedule, delegate, board, browser]
```

What a tool may do on its own is decided by the agent's [permission mode and approvals](../agents/approvals.md).

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
| Talk out loud, or answer voice notes with voice | `realtime:`, `habakkuk talk`, `tts:` | [Voice](voice.md) |
| Sit in a video call and take notes | `meeting` | [Meetings](meetings.md) |
| Use GitHub, Linear, Notion, a database and hundreds more | `mcp:<server>` | [MCP](mcp.md) |
| Install someone's packaged skills, tools and commands | `habakkuk plugins install` | [Plugins](plugins.md) |
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

An agent with a [home chat](../agents/multi-agent.md#teams-and-home-chats) also gets `message_home`.

## Tool search

An agent whose toolsets hold more tools than `tool_search_over` (default 60; `-1` turns it off), usually from MCP servers, is offered only the `core` tools plus `tool_search` and `tool_describe`. The tools it finds are added for the rest of the session.

```yaml
agents:
  habakkuk:
    tool_search_over: 60
```
