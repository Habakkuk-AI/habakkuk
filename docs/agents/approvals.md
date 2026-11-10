---
title: Permissions and approvals
description: "How permission modes and approvals work in chats and the terminal, plus the reviewer model, hooks that can block a tool call, and the guards that always apply."
order: 7
---

Some things an agent may do on its own. For the rest, somebody has to say yes. Every tool declares a risk: `read`, `write`, `network` or `exec`. The agent's permission mode looks at that risk and decides whether a call runs, needs approval or is denied. When a call needs approval, the approvals setting decides who says yes.

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

- The tools that write memory and skills only touch the agent's own folder under `$HABAKKUK_HOME/agents/<name>`. That's why `workspace` mode runs them without asking.
- Each MCP server declares a risk for its tools (the default is `network`). Tools listed in a server's `approve` always need approval in any mode below `full`. See [MCP](../tools/mcp.md).
- The mode isn't the only check. Turns that come from chat apps, and turns that have read outside content, need approval for risky calls even in `full` mode. See [Security](../deploy/security.md).

## Answering approvals

- **In a chat**, Habakkuk says in everyday words what it wants to do and why it's asking. Then it waits for the owner to answer. Platforms with buttons show **Allow once**, **Always allow** and **Deny**. Elsewhere you reply **yes**, **always** or **no**. Reply **details** to see the exact command. The request keeps waiting. If there's no answer after 15 minutes, Habakkuk skips the step. See [what the request looks like](../channels/settings.md#approval-requests-in-plain-words) and [Always allow](#always-allow).
- **In the terminal UI**, a prompt lets you allow the call once, allow it for the session, or deny it.
- **In the dashboard and desktop app**, the Approvals page lets you answer approvals that are waiting in any chat. It also lists each agent's saved rules, and you can remove one there.
- **Scheduled runs** ask you in the chat the schedule delivers to, and wait as a chat does. See [Approvals in scheduled runs](#approvals-in-scheduled-runs).
- **Other unattended runs** have nobody to ask. These are sub-agents, board cards, webhooks and the OpenAI-compatible API. Calls that need approval are denied there, unless a reviewer decides or a [saved rule](#always-allow) covers them.

### Approvals and restarts

Sometimes `hk serve` stops while a request is waiting, for an update, a deploy or a reboot. The request isn't lost, and nothing answers it by accident:

- The turn resumes when `serve` starts. When it gets back to the same call, it asks again with a new request.
- A button on the old request answers the new one only if both are for the same call. That means the same agent, conversation, tool and input. Press any other old button and you get "That request has expired", with a pointer to the request that's waiting now.
- If you type **yes** or **no** before the turn has asked again, you get the same notice. Habakkuk doesn't pass your reply to the agent as a new message.
- A planned stop doesn't count against the turn. When `serve` is told to stop, it hands the job back. The job resumes at once and keeps all its attempts. A crash still counts as an attempt.

## Approvals

When the mode says a call needs approval, `approvals` decides who answers:

```yaml
agents:
  habakkuk:
    approvals: reviewer_then_human   # human (default) | reviewer | reviewer_then_human
    reviewer_model: anthropic/claude-sonnet-5-5   # default: the agent's own chain
```

- **`human`** (default): you answer in the chat or the terminal. In a run that nobody is watching, the call is denied.
- **`reviewer`**: a model decides in your place. Habakkuk makes one model call with a fixed, strict prompt, and shows it the task, the tool call and the workspace. The reviewer allows routine work. It denies actions that destroy things or send data out, such as `rm -rf` outside the workspace, `curl | sh`, sending secrets to the network or editing system files. If the reviewer is unsure, the call is denied.
- **`reviewer_then_human`**: this works like `reviewer`, except that a call the reviewer is unsure about goes to you, if you're in a chat.

| `approvals` | Reviewer says allow | deny | unsure |
|---|---|---|---|
| `reviewer` | runs | denied | denied |
| `reviewer_then_human` | runs | denied | goes to the chat or terminal; denied when there is none |

Habakkuk logs every decision and sends the reason back to the agent, so the agent can adapt. For the reviewer's prompt and how its answer is parsed, see [Architecture: approval reviewer](../reference/architecture.md#approval-reviewer).

> [!IMPORTANT]
> Habakkuk never asks the reviewer about a turn that has read outside content, because the same text could steer the reviewer model too. Those calls go to a human. See [Security](../deploy/security.md#outside-content-taints-a-turn).

## Always allow

Answering the same request every day gets old. When you pick **Always allow** (or reply **always**), Habakkuk saves a rule for that agent and stops asking about calls like this one. The request tells you what the rule will cover before you answer, for example:

> **Always allow** saves a rule, so I won't ask again for `git status` run in your workspace.

A rule covers one of these:

- **A command:** a program and its arguments, run in the workspace or in the folder the command ran in. `git status` covers `git status` and nothing else. Add `*` at the end in the config to allow any further arguments: `npm run *`.
- **An exact command line:** a command with pipes, `&&`, redirects or other shell syntax is saved exactly as it was. Only that same line is covered.
- **A file tool in a folder:** `write_file`, `edit_file` or `apply_patch` writing inside the folder of the file you approved.

Patterns only match plain commands. A command with `;`, `&&`, `|`, redirects, `$`, backquotes, wildcards, `~` or a variable set in front of it is never covered by a pattern, and neither is quoting that doesn't close. When in doubt, Habakkuk asks.

You can also write rules in the config:

```yaml
agents:
  habakkuk:
    allow:
      - command: git status              # this program with these arguments
      - command: npm run *               # * at the end: any further arguments
        in: ~/code/shop                  # where it may run (default: the workspace)
      - exact: make build && make test   # this exact command line
      - tool: write_file                 # a file tool...
        path: ~/Documents/reports        # ...writing inside this folder
```

Saved rules live in `approvals.json` in the agent's folder and apply across restarts. Run `hk approvals list` to see every rule and `hk approvals rm ID` to remove a saved one. Remove a config rule by editing the config.

> [!IMPORTANT]
> A rule never answers in these cases, whatever it says:
>
> - **A turn that has read outside content.** A web page, search result, email, MCP tool, webhook, Home Assistant payload or device output could have written the command. Those calls go to a person, and **Always allow** isn't offered. See [Security](../deploy/security.md#outside-content-taints-a-turn).
> - **A guest's turn.** Guests get no risky capability at all.
> - **A command on the hardline blocklist.** Those never run. See [Safety guards](#safety-guards).
> - **A call that always asks**, such as a write to the agent's instruction files. See [Instruction files](#instruction-files).

Rules sit next to the permission mode. They answer only for calls that would otherwise ask you. In `read-only` mode nothing runs, rule or not.

## Approvals in scheduled runs

A scheduled run has no chat of its own. When one of its calls needs approval, Habakkuk asks you in the chat the schedule delivers to, says which scheduled task it's for, and waits up to the same 15 minutes. Your answer works the same way as in any chat, **Always allow** included. The next run then goes ahead without asking.

Some runs still can't get a yes:

- A schedule with no delivery chat has nobody to ask, so the call is denied.
- A run on an account that's open to everyone (`allow_from: ["*"]`) counts as a guest's turn, so risky calls are refused.
- A run that has read outside content asks you every time. No rule covers it.

## Instruction files

`SOUL.md`, `AGENTS.md`, `IDENTITY.md` and `USER.md` tell the agent who it is and how to behave. A write to a file with one of those names, through `write_file`, `edit_file` or `apply_patch`, always needs your approval. That holds in `full` mode, in a trusted notes folder, under a rule, and with a reviewer. The same goes for the agent's saved rules file. **Always allow** isn't offered for these, and no rule covers a command that names one of them.

The tools that keep the user profile and the agent's identity have their own limits and aren't changed by this. A turn that has read outside content still can't use them.

## Trusted folders for notes

A turn that comes from a chat, or that has read outside content, needs your approval for every file it writes outside the workspace. If the agent keeps notes for you in a folder, that's a lot of asking. List folders like that in `trusted_write_paths`:

```yaml
agents:
  habakkuk:
    trusted_write_paths: [~/notes, ~/Documents/meeting-notes]
```

In those folders, `write_file`, `edit_file` and `apply_patch` write without asking. That holds for a turn from a chat and for a turn that has read outside content. The permission mode still applies, so `read-only` writes nothing and `ask` asks about every write.

The setting is narrow on purpose:

- **Commands aren't covered.** Running `exec` in a trusted folder, or into one, needs approval as before. So nothing written there can run without you.
- **No scripts or programs.** A file counts as a script or program if it has an executable bit, a script extension (`.sh`, `.py`, `.js` and so on) or a `#!` first line, or if it's a program binary. Habakkuk refuses such a file in a turn that has read outside content. In a clean turn, the file goes through the usual approval. Skills use the same test for "ships scripts".
- **No hidden files.** `.git`, `.envrc` and anything else whose name starts with a dot aren't covered. Neither is anything that sits under a folder like that.
- **Symlinks are followed first.** Habakkuk judges a path by where it really lands, so a link inside the folder can't send a write somewhere else.
- **Habakkuk's own folders are never trusted.** These are agent homes, skills, plugins, hooks, personas, channel credentials and backups, plus the folders your hooks and scheduled scripts run from. If an entry reaches one of them, Habakkuk ignores it and `hk security` says so.
- **Your home folder, or anything above it, is refused.** Habakkuk rejects it when the config loads.
- **Guests get nothing.** A sender who was let in by `allow_from: ["*"]` can't write outside the workspace at all.

Run `hk security` to list the trusted folders for each agent. [Security](../deploy/security.md#trusted-folders-for-notes) describes the trade-off.

## Hooks

A hook is a command of yours that Habakkuk runs when an event happens. You list hooks under `hooks:`. The events are `turn_start`, `turn_end`, `tool_before`, `tool_after`, `message_in`, `message_out`, `session_new` and `schedule_run`. The command gets the event as JSON on stdin, with `HABAKKUK_EVENT` set.

A `tool_before` hook can block a tool call. If the hook exits non-zero, the call is blocked and the hook's stdout is the reason. A hook that times out or can't start blocks the call too.

```yaml
hooks:
  - event: tool_before
    match: ^exec$                    # a regular expression on the tool name
    command: ~/.habakkuk/hooks/check-exec.sh
    timeout: 5s
  - event: turn_end
    command: cat >> ~/.habakkuk/turns.jsonl
```

Changes to hooks apply without a restart. Plugins can add hooks too. See [Plugins](../tools/plugins.md#manifest).

## Safety guards

Habakkuk never runs some commands, whatever the permission mode. They are recursive deletion of `/` or your home, `mkfs`, `dd` onto a disk, fork bombs, and a remote download piped into a shell.

`fetch_url` refuses loopback, LAN and cloud metadata addresses. To allow them, set `tools.allow_private_urls`:

```yaml
tools: {allow_private_urls: false}   # fetch_url and A2A refuse loopback, LAN and metadata addresses
```

Content from outside marks the turn as tainted. That covers web pages, search results, network tools and an email inbox. A tainted turn can't write trusted memory, change the user profile or set the agent's identity.
