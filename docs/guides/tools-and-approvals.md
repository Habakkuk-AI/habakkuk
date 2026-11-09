---
title: Tools and approvals
description: "Toolsets, permission modes, approvals and the reviewer, workspace tools, background processes, sandboxes, checkpoints, hooks and safety guards."
order: 7
---

## Toolsets

Tools come in toolsets, and an agent lists the ones it gets under `toolsets`. Without the setting it gets `core`, `memory`, `skills`, `schedule`, `delegate` and `board`.

| Toolset | Tools | Guide |
|---|---|---|
| `core` | `read_file`, `write_file`, `edit_file`, `list_dir`, `exec`, `apply_patch`, `search_files`, `fetch_url`, background processes, `todo`, `clarify`; with channels `send_file` and `poll`; media tools when configured | below |
| `memory` | `memory_save`, `memory_search`, `memory_read`, `memory_forget`, `user_profile_read`, `user_profile_update`, `session_search` | [Memory and skills](memory-and-skills.md) |
| `skills` | `skill_view`, `skill_file`, `skill_save` | [Memory and skills](memory-and-skills.md) |
| `schedule` | `schedule_create` and friends | [Schedules and the task board](schedules-and-board.md) |
| `delegate` | `delegate` | [Multi-agent teams](multi-agent.md) |
| `board` | `board_*` | [Schedules and the task board](schedules-and-board.md) |
| `browser`, `computer`, `meeting` | Browser, desktop and meeting tools | [Browser and computer use](browser-and-computer.md), [Meetings](meetings.md) |
| `mcp:<server>` | One per MCP server | [MCP](mcp.md) |
| `a2a`, `homeassistant` | Other agents, Home Assistant | [Integrations](integrations.md) |

## Permission modes

Each agent runs in one mode:

| Mode | Reads | File writes | Exec / network |
|---|---|---|---|
| `read-only` | allowed | denied | denied |
| `ask` | allowed | approval | approval |
| `workspace` (default) | allowed | allowed inside the workspace | approval |
| `full` | allowed | allowed | allowed |

Every tool declares a risk (`read`, `write`, `network` or `exec`) and the mode decides from it. The memory and skill write tools only touch the agent's own folder under `$HABAKKUK_HOME/agents/<name>`, so `workspace` mode runs them without asking. Unattended runs (no approver attached) deny anything that needs approval. In a chat channel the approver is the chat: Habakkuk posts the request and waits for "yes" or "no" from an allowed sender (15 minutes, then it skips the step). Platforms with buttons show Approve and Deny.

## Approvals

In `workspace` mode, running programs (and writing outside the workspace) needs approval. With `approvals: human` you answer in the chat or terminal, and runs with nobody watching (schedules, sub-agents) are denied. `approvals: reviewer` lets a model decide instead: one call with a fixed, strict prompt that sees the task, the tool call and the workspace, allows routine work, and denies destructive or exfiltrating actions such as `rm -rf` outside the workspace, `curl | sh`, sending secrets to the network or editing system files. When it is unsure, the call is denied, or with `reviewer_then_human` handed to you if you are in a chat. Every decision is logged, and its reason goes back to the agent so it can adapt.

| `approvals` | Reviewer says allow | deny | ask (unsure) |
|---|---|---|---|
| `reviewer` | runs | denied | denied |
| `reviewer_then_human` | runs | denied | goes to the chat or terminal; denied when there is none |

`reviewer_model` picks the reviewer's model (default: the agent's own chain). [Architecture: approval reviewer](../reference/architecture.md#approval-reviewer) describes its prompt and parsing.

## Working in the workspace

Besides the file tools and `exec`, `core` has `apply_patch` (many files at once, from a unified diff or the `*** Begin Patch` format; every hunk is applied in memory before anything is written) and `search_files` (a regular expression search, with `rg` when it is installed).

**Background processes.** `exec` with `background: true` starts the command and returns a process id at once; `process_list`, `process_output`, `process_wait`, `process_kill` and `process_write` act on it. Output goes to `$HABAKKUK_HOME/procs/<id>.log`, capped. With `notify: true`, a process that exits starts a turn that tells the agent. Processes live as long as the Habakkuk process.

## Sandboxes

An agent's `sandbox` decides where `exec` and background processes run:

| `sandbox` | Commands run | File tools |
|---|---|---|
| `none` (default) | here, with bash, each in its own process group | the local workspace |
| `docker` | in one long-lived container per agent (`habakkuk-<agent>`) | the local workspace, which is bind-mounted at the same path |
| `ssh` | on a remote host, in `workdir` | reads stay local; `write_file`, `edit_file` and `apply_patch` are refused; change files there with `exec` |

```yaml
agents:
  coder:
    sandbox: docker
    docker: {image: "debian:stable-slim", network: false, cpus: "2", memory: 2g}
  remote:
    sandbox: ssh
    ssh: {host: build.example.com, user: habakkuk, key_env: BUILD_SSH_KEY, workdir: /srv/work}
```

Both drive the `docker` and `ssh` command-line tools. See [Architecture: sandboxes](../reference/architecture.md#sandboxes).

## Checkpoints

Before the first call in a turn that may change the workspace, Habakkuk snapshots it into a shadow git repository in `$HABAKKUK_HOME/checkpoints/<agent>`; the workspace's own `.git` is never touched. It needs git on `PATH`; `checkpoints: false` turns it off and `checkpoint_max_mb` (default 200) caps a snapshot.

```bash
habakkuk checkpoints [-agent A]            # workspace snapshots taken before turns changed files
habakkuk rollback [-agent A] [-y] ID [PATH] # restore the workspace, or one path, to a snapshot
```

`/undo` in a chat restores the workspace to before the conversation's latest turn that changed it, and walks further back when repeated.

## Tool search

An agent whose toolsets hold more tools than `tool_search_over` (default 60; `-1` turns it off), usually from MCP servers, is offered only the `core` tools plus `tool_search` and `tool_describe`, and the tools it finds are added for the rest of the session.

## Hooks

`hooks:` runs your commands on events: `turn_start`, `turn_end`, `tool_before`, `tool_after`, `message_in`, `message_out`, `session_new` and `schedule_run`. The command gets the event as JSON on stdin and `HABAKKUK_EVENT` set. A `tool_before` hook that exits non-zero blocks the call, and its stdout is the reason; a hook that times out or cannot start also blocks.

```yaml
hooks:
  - event: tool_before
    match: ^exec$
    command: ~/.habakkuk/hooks/check-exec.sh
    timeout: 5s
  - event: turn_end
    command: cat >> ~/.habakkuk/turns.jsonl
```

## Media tools

With the matching config sections, agents get:

- `view_image` and `read_document` (PDF, DOCX, XLSX, PPTX, HTML) for files in the workspace;
- `image_generate` (`images:` with `openai`, `gemini` or `fal`) and `video_generate` (`video:` with the same providers);
- `web_search` (`search:` with `brave`, `tavily`, `exa`, `searxng` or `duckduckgo`; DuckDuckGo, best effort, without a section);
- `speak` (`tts:` with `openai`, `elevenlabs` or `edge`); see [Voice](voice.md).

```yaml
images:
  provider: openai
  key_env: OPENAI_API_KEY
  model: gpt-image-2
search:
  provider: brave
  key_env: BRAVE_API_KEY
```

[Architecture: media](../reference/architecture.md#media) has every option.

## Safety guards

Some commands are never run, in any permission mode: recursive deletion of `/` or your home, `mkfs`, `dd` onto a disk, fork bombs, and a remote download piped into a shell. `fetch_url` refuses loopback, LAN and cloud metadata addresses unless `tools.allow_private_urls` is set:

```yaml
tools: {allow_private_urls: false} # fetch_url refuses loopback, LAN and metadata addresses
```

Content from outside (web pages, search results, network tools, an email inbox) marks the turn as tainted, so it cannot write trusted memory, change the user profile or set the agent's identity.
