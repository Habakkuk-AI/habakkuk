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

- **In a chat**, Habakkuk says what it wants to do in everyday words, why it is asking, and waits for **yes** or **no** from the owner. Platforms with buttons show Approve and Deny. Reply **details** to see the exact command; the request keeps waiting. After 15 minutes without an answer it skips the step. See [what the request looks like](../channels/settings.md#approval-requests-in-plain-words).
- **In the terminal UI**, a prompt offers allow once, allow for the session, or deny.
- **In the dashboard and desktop app**, approvals waiting in any chat can be answered from the Approvals page.
- **Unattended runs** (schedules, sub-agents, webhooks, the OpenAI-compatible API) have nobody to ask, so calls that need approval are denied, unless a reviewer decides.

### Approvals and restarts

A request that is waiting when `habakkuk serve` stops (an update, a deploy, a reboot) is not lost and not answered by accident:

- The turn resumes when `serve` starts. When it reaches the same call again it asks again, with a new request.
- A button on the old request answers the new one only when it is for the same call: the same agent, conversation, tool and input. A press on any other old button gets "That request has expired", and points at the one waiting now.
- A typed **yes** or **no** that arrives before the turn has asked again gets the same notice. It is not passed to the agent as a new message.
- A planned stop does not count against the turn. `serve` hands the job back when it is told to stop, so it resumes at once and keeps all its attempts. A crash still counts as an attempt.

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

## Trusted folders for notes

A turn from a chat, or one that read outside content, needs your approval for every file it writes outside the workspace. For a folder where the agent keeps notes for you, that is a lot of asking. `trusted_write_paths` names such folders:

```yaml
agents:
  habakkuk:
    trusted_write_paths: [~/notes, ~/Documents/meeting-notes]
```

In those folders `write_file`, `edit_file` and `apply_patch` write without asking, from a chat and after reading outside content. The permission mode still applies: `read-only` writes nothing and `ask` asks about every write.

It is kept narrow on purpose:

- **Commands are not covered.** `exec` in or into a trusted folder needs approval as before, so nothing written there can be run without you.
- **No scripts or programs.** A file with an executable bit, a script extension (`.sh`, `.py`, `.js` and so on), a `#!` first line or a program binary is refused in a turn that read outside content. In a clean turn it goes through the usual approval. This is the same test skills use for "ships scripts".
- **No hidden files.** `.git`, `.envrc` and anything else whose name starts with a dot, or that sits under such a folder, is not covered.
- **Symlinks are followed first.** A path is judged by where it really lands, so a link inside the folder cannot send a write somewhere else.
- **Habakkuk's own folders are never trusted**: agent homes, skills, plugins, hooks, personas, channel credentials, backups, and the folders your hooks and scheduled scripts run from. An entry that reaches them is ignored and `habakkuk security` says so.
- **Your home folder, or anything above it, is refused** when the config loads.
- **Guests get nothing.** A sender let in by `allow_from: ["*"]` cannot write outside the workspace at all.

`habakkuk security` lists the folders for each agent. The trade-off is described in [Security](../deploy/security.md#trusted-folders-for-notes).

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
