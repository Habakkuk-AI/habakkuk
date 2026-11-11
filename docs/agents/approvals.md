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
- **Scheduled runs** ask you in the chat the schedule delivers to, and wait as a chat does. Saved rules never answer for them. See [Approvals in scheduled runs](#approvals-in-scheduled-runs).
- **Other unattended runs** have nobody to ask. These are sub-agents, board cards, webhooks, tasks from other agents (A2A), MCP clients and the OpenAI-compatible API. Calls that need approval are denied there, unless a reviewer decides. [Saved rules](#always-allow) never answer for them.

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

Answering the same request every day gets old. When you pick **Always allow** (or reply **always**), Habakkuk saves a rule and stops asking about this call when you ask for it again. Before you answer, the request shows the rule in full, exactly as it will be saved:

````text
Always allow saves a rule for this chat. I won't ask again before running exactly this command in your workspace:
```
git status
```
````

A rule covers one of these:

- **One simple command, in one folder.** A program followed by plain words, such as `git status` or `ls ~/notes`. The rule allows that exact line, run in the folder it ran in, and nowhere else. A subfolder is another folder: `ls`, `make` or `npm test` there read other files. Quoting may differ (`echo 'a b'` is `echo "a b"`), nothing else may: `git status -s` isn't `git status`. Habakkuk also notes where the program was found, and a program found somewhere else, or swapped for a link, isn't covered. Rules that take any arguments (wildcards) may come later, with a reviewed list of programs they're safe for.
- **A file tool in a folder.** `write_file`, `edit_file` or `apply_patch` writing inside the folder of the file you approved, or below it. Hidden files and folders still ask.

A command isn't simple, and gets no **Always allow**, if it has an unquoted `$`, a backquote, `{` or `}`, `*`, `?` or `[`, a `~` other than an unquoted leading `~/` (quotes in front count: `''"~/x"` is refused), a word starting with `=`, any of `;`, `&`, `|`, `<`, `>`, `(` or `)`, a line break, or runs `cd`, `pushd` or `popd`. **Always allow** also needs the program to be found where it is now (a program that's missing isn't offered a rule). You can still allow any of these once.

**Which shell.** Command rules are judged for bash, sh and dash, run on this computer. Exec rules, saved or from the config, are never offered or applied when:

- the agent runs commands in a Docker or SSH sandbox, whose programs this computer can't pin;
- Habakkuk runs on Windows, where commands run in PowerShell;
- `HABAKKUK_SHELL` names another shell: zsh (it expands a leading `=`), PowerShell, or a path to fish or any other program. A bare name Habakkuk doesn't know, such as `fish`, is ignored and bash runs.

File rules are unaffected.

**Workspace inside the home.** An agent whose workspace is inside Habakkuk's home never gets command or file rules there. That's the default when the agent has no `workspace:` (it's `agents/<name>/workspace`). To use rules, give it a workspace outside the home:

```yaml
agents:
  habakkuk:
    workspace: ~/habakkuk-workspace
```

### Which turns rules answer for

A rule answers only for a **typed** turn: one whose whole message is plain text you just sent. That's one of these:

- a text message in a one-to-one chat on **Telegram, WhatsApp (linked device or Cloud API) or the WhatsApp agent channel**;
- a line you type at the `hk chat` or TUI prompt, when `hk chat` reads from a terminal;
- a message typed into the dashboard's chat. That includes `/api/chat`, which is the same chat behind the dashboard's token.

Those are the channels whose platforms tell Habakkuk when a message is forwarded or quotes another, so a plain message there really is your typing. On every other channel no turn is typed and rules never answer: Slack, Discord, Signal, Matrix, Microsoft Teams, Google Chat, email, SMS, ntfy, IRC, Zulip, DingTalk, WeCom and the rest.

None of these are typed, even when you started them:

- `/btw` in the TUI, which carries the conversation's history;
- `/skill`, plugin commands and `hk run -skill`, which carry a file's text, and `hk run` itself;
- `hk send`, a retry, `/queue` and `/goal` follow-ups;
- Live mode, voice notes and dashboard recordings (wherever they're transcribed), and `hk talk`, since speech could be anyone's;
- a message that quotes another message (a reply across chats included), a forwarded message, a Telegram message sent through an inline bot, on behalf of a chat, or by Telegram Business while you're away (away, greeting and scheduled messages), a WhatsApp template button's text or ad referral, and a message with an attachment;
- `hk chat` reading from a pipe rather than a terminal.

A typed turn also needs all of these:

- **The conversation is yours alone.** Not a group, room or channel you share with others.
- **The turn is untainted.** It hasn't read outside content.
- **Your message carries no chat notes.** Nothing reached the chat from elsewhere since your last message there: no schedule's result, script output, meeting notes, card result or `hk send` message.
- **The conversation isn't paused.** See [When rules pause](#when-rules-pause).

Rules never answer for anything else, and those calls ask a person as before:

- schedules of any kind, prompt or script;
- notices, board cards and sub-agents;
- tasks from other agents (A2A), the OpenAI-compatible API, MCP clients and editors (`hk acp`);
- webhooks, guests' turns, and turns from a chat account the config no longer has.

**Where a rule applies.** A rule you save in a chat applies in that chat only. A rule you save on this computer (the dashboard's chat) applies to messages you type on this computer. Rules in the config apply wherever rules apply. Each saved rule records who answered.

### When rules pause

Your message gets no rules when anything but your own words entered the conversation since your previous typed message. That covers:

- in your previous message's own turn: a tool that read outside content, or chat notes that came in with it;
- after it: any turn you didn't type (a finished process, meeting notes, a sub-agent's result, a schedule's run, an A2A task, a `/goal` follow-up, a retry), any chat note, and a Live call's transcript, which the dashboard writes into the conversation when the call ends.

Once a typed message of yours has gone through with nothing like that in or after it, the next one gets its rules back.

### What a rule won't match

- A command that names an instruction file or the rules file, however it's quoted, escaped or cased. See [Instruction files](#instruction-files).
- A command whose words lead to one of those files through a symlink or a hard link, as they are when it runs. Short options such as `-onotes.md` and values after `=` are read as paths too.
- A command run in Habakkuk's home or one of its own folders, or one that names a path there, even a file that doesn't exist yet.
- A command on the hardline blocklist.

**Always allow** isn't offered for a command or folder with a backquote or a line break, or one longer than about 300 characters. It isn't offered in an email conversation either.

### Rules in the config

```yaml
agents:
  habakkuk:
    allow:
      - exact: git status                # this command line, in the workspace
      - exact: make test                 # one simple command
        in: ~/code/shop                  # the folder it runs in, exactly
      - tool: write_file                 # a file tool...
        path: ~/Documents/reports        # ...writing inside this folder
```

An `exact` line must be a simple command, and the config doesn't load otherwise. A `command:` pattern is refused for now.

Saved rules live in `approvals.json` in the agent's folder and apply across restarts. Run `hk approvals list` to see every rule, where it applies and who saved it, and `hk approvals rm ID` to remove a saved one. Remove a config rule by editing the config.

> [!IMPORTANT]
> A rule never answers for a turn that has read outside content. A web page, search result, email, MCP tool, webhook, Home Assistant payload, another agent's task or device output could have written the command. Those calls go to a person, and **Always allow** isn't offered. See [Security](../deploy/security.md#outside-content-taints-a-turn).

Rules sit next to the permission mode. They answer only for calls that would otherwise ask you. In `read-only` mode nothing runs, rule or not.

On an account that's open to everyone, a guest's turn is refused every risky capability. Calls that use none, such as a network tool in `ask` mode, still ask in the chat, and only you can answer. **Always allow** isn't offered for a guest's turn.

The known limits are listed under [Security](../deploy/security.md#saved-approval-rules).

## Approvals in scheduled runs

A scheduled run has no chat of its own. When one of its calls needs approval, Habakkuk asks you in the chat the schedule delivers to, says which scheduled task it's for, and waits up to the same 15 minutes. You answer yes or no as in any chat. Saved rules never answer for a scheduled run, and **Always allow** isn't offered there, so each run asks.

While a run is still going, perhaps waiting for your answer, the schedule's next slot is skipped. A chat shows one request at a time. A request that can't get its turn within the approval timeout is denied, so a busy chat never holds up a worker for good.

Some runs can't get a yes:

- A schedule with no delivery chat has nobody to ask, so the call is denied.
- A run on an account that's open to everyone (`allow_from: ["*"]`) counts as a guest's turn, so risky calls are refused.
- A run that has read outside content asks you every time, as does every run of a schedule that wasn't made by a clean message you typed, and every turn that reads a script's output.

## Instruction files

`SOUL.md`, `AGENTS.md`, `IDENTITY.md` and `USER.md` tell the agent who it is and how to behave. A write to a file with one of those names, through `write_file`, `edit_file` or `apply_patch`, always needs your approval. Names are compared without regard to case, since macOS and Windows disks ignore it. That holds in `full` mode, in a trusted notes folder, under a rule, and with a reviewer. The same goes for any `approvals.json` file and its lock, whatever case its name is written in, for Habakkuk's own `config.yaml`, `.env` and databases in its home, and for any file that has more than one name (a hard link), or whose names can't be counted. **Always allow** isn't offered for these.

No rule covers a command that names one of these files, however it's quoted, escaped or cased: `SOU''L.md`, `"AGENTS".md` and `ſOUL.md` count, and so does a word that leads to one through a symlink or a hard link. In `full` mode, though, commands run without asking, and Habakkuk doesn't parse them, so a command can still write these files there. Below `full`, such a command asks you.

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
- **Habakkuk's own folders are never trusted.** These are agent homes, skills, plugins, hooks, personas, channel credentials, backups and scripts, plus the folders your hooks and scheduled scripts run from. If an entry reaches one of them, Habakkuk ignores it and `hk security` says so.
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
