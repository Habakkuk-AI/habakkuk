---
title: Permissions and approvals
description: "Permission modes, how approvals work in chats and the terminal, the approval reviewer model, hooks that can block a tool call, and the guards that always apply."
order: 7
---

Every tool declares a risk: `read`, `write`, `network` or `exec`. An agent's permission mode decides from that risk whether a call runs, needs approval, or is denied. Approvals then decide who says yes.

## Permission modes

```yaml
agents:
  habakkuk:
    permission: workspace     # read-only | ask | workspace (default) | full
```

| Mode | Reads | File writes | Network | Exec |
|---|---|---|---|---|
| `read-only` | allowed | denied | denied | denied |
| `ask` | allowed | approval | approval | approval |
| `workspace` (default) | allowed | allowed inside the workspace, approval outside | allowed | approval |
| `full` | allowed | allowed | allowed | allowed |

- The memory and skill write tools only touch the agent's own folder under `$HABAKKUK_HOME/agents/<name>`, so `workspace` mode runs them without asking.
- MCP servers declare a risk for their tools (default `network`); tools listed in a server's `approve` always need approval below `full`. See [MCP](../tools/mcp.md).
- On top of the mode, turns from chat apps and turns that read outside content need approval for risky calls even in `full` mode. See [Security](../deploy/security.md).

## Answering approvals

- **In a chat**, Habakkuk posts the request and waits for **yes** or **no** from an allowed sender. Platforms with buttons show Approve and Deny. After 15 minutes without an answer it skips the step.
- **In the terminal UI**, a prompt offers allow once, allow for the session, or deny.
- **In the dashboard and desktop app**, approvals waiting in any chat can be answered from the Approvals page.
- **Unattended runs** (schedules, sub-agents, webhooks, the OpenAI-compatible API) have nobody to ask, so calls that need approval are denied, unless a reviewer decides.

## Approvals

`approvals` decides who answers when the mode says a call needs approval:

```yaml
agents:
  habakkuk:
    approvals: reviewer_then_human   # human (default) | reviewer | reviewer_then_human
    reviewer_model: anthropic/claude-sonnet-5-5   # default: the agent's own chain
```

- **`human`** (default): you answer in the chat or terminal. Runs with nobody watching are denied.
- **`reviewer`**: a model decides instead. One call with a fixed, strict prompt sees the task, the tool call and the workspace. It allows routine work, and denies destructive or exfiltrating actions such as `rm -rf` outside the workspace, `curl | sh`, sending secrets to the network or editing system files. When it is unsure, the call is denied.
- **`reviewer_then_human`**: as `reviewer`, but an unsure call goes to you if you are in a chat.

| `approvals` | Reviewer says allow | deny | unsure |
|---|---|---|---|
| `reviewer` | runs | denied | denied |
| `reviewer_then_human` | runs | denied | goes to the chat or terminal; denied when there is none |

Every decision is logged, and its reason goes back to the agent so it can adapt. [Architecture: approval reviewer](../reference/architecture.md#approval-reviewer) describes its prompt and parsing.

> [!IMPORTANT]
> The reviewer is never asked about a turn that read outside content: a model could be steered by the same text. Those calls go to a human. See [Security](../deploy/security.md#outside-content-taints-a-turn).

## Hooks

`hooks:` runs your commands on events: `turn_start`, `turn_end`, `tool_before`, `tool_after`, `message_in`, `message_out`, `session_new` and `schedule_run`. The command gets the event as JSON on stdin, with `HABAKKUK_EVENT` set.

A `tool_before` hook that exits non-zero blocks the call, and its stdout is the reason. A hook that times out or cannot start also blocks.

```yaml
hooks:
  - event: tool_before
    match: ^exec$                    # a regular expression on the tool name
    command: ~/.habakkuk/hooks/check-exec.sh
    timeout: 5s
  - event: turn_end
    command: cat >> ~/.habakkuk/turns.jsonl
```

Hook changes apply without a restart. Plugins can add hooks too; see [Plugins](../tools/plugins.md#manifest).

## Safety guards

Some commands are never run, in any permission mode: recursive deletion of `/` or your home, `mkfs`, `dd` onto a disk, fork bombs, and a remote download piped into a shell.

`fetch_url` refuses loopback, LAN and cloud metadata addresses unless `tools.allow_private_urls` is set:

```yaml
tools: {allow_private_urls: false}   # fetch_url and A2A refuse loopback, LAN and metadata addresses
```

Content from outside (web pages, search results, network tools, an email inbox) marks the turn as tainted, so it cannot write trusted memory, change the user profile or set the agent's identity.
