---
title: CLI
description: "Every habakkuk command with its flags, and every slash command you can send in a chat."
order: 1
---

Every command you can run in a terminal is here, with its flags, along with every slash command you can send in a chat. Come here when you know what you want to do and need the exact spelling.

`habakkuk help` prints this list. Run `habakkuk` with no arguments and you get a short screen instead. On a new computer, it tells you what Habakkuk is and the one command to run next (`habakkuk setup`). Once you're set up, it tells you whether `serve` is running and shows the everyday commands. `hk` is the short name. The installers put it beside `habakkuk`, and every command below works with either (`hk chat`, `hk doctor`). `HABAKKUK_HOME` selects the home that every command works on. The default is `~/.habakkuk`, or `%APPDATA%\Habakkuk` on Windows.

You can run any command on another machine. `hk -r NAME COMMAND` runs it on a saved remote. Over ssh that works for every command, and over HTTPS for `status` and `chat`. See [Remote CLI](../interfaces/remote-cli.md).

## Global flags

Put these before the command.

| Flag | |
|---|---|
| `-r NAME`, `--remote NAME` | Run the command on that remote. `HABAKKUK_REMOTE=NAME` and `hk remote default NAME` do the same without the flag; `-r local` runs here. |
| `-q`, `--quiet` | Do not print the `(remote: NAME)` line. |
| `--yes-remote` | Allow `serve`, `init`, `restore`, `update`, `migrate`, `setup service` and `config migrate` on a remote. |

## Commands

```text
habakkuk init                       write a starter config to $HABAKKUK_HOME (default ~/.habakkuk)
habakkuk setup [model|channel [TYPE]|name|service] [-y] [flags]
                                    a one-minute wizard: model, where to talk (WhatsApp agent,
                                    Telegram, web, ...), names; -y runs it from flags (habakkuk setup -h);
                                    on a new home it offers to import another agent (-y -migrate does it)
habakkuk setup channel NAME [-progress plain|detailed|off] [-approvals-detail summary|full]
                                    change only what an existing channel shows in its chats while the
                                    agent works and in approval requests; serve picks it up, no restart
habakkuk migrate [-from SOURCE] [-source DIR] [-agent NAME] [-dry-run] [-secrets copy|skip]
                 [-files copy|skip] [-files-dir DIR] [-files-max SIZE] [-force] [-y]
                                    bring agents, persona files, profile, memory, skills, schedules
                                    (paused), MCP servers, providers, channels and keys over from
                                    another agent; -files copy also copies your own files that the
                                    imported text names into $HABAKKUK_HOME/files and rewrites the
                                    text to name the copies (see Move from another agent)
habakkuk chat [-agent A] [-session ID] [-resume] [-plain]
                                    interactive conversation: the terminal UI on a terminal,
                                    line by line with -plain or when piped
habakkuk tui [-agent A] [-session ID] [-resume]
                                    the full-screen terminal UI (-resume picks a recent session)
habakkuk talk [-agent A] [-session ID] [-wake "hey habakkuk"] [-no-wake]
                                    a voice loop on this machine: the mic through ffmpeg, replies
                                    spoken with tts; talk over a reply to stop it
habakkuk run [-agent A] [-skill NAME] PROMPT
                                    one turn, print the answer, exit (-skill: follow that skill)
habakkuk serve                      run workers: resumes interrupted turns, applies retention;
                                    with an http section, also the chat page, API and webhooks
habakkuk channels                   show channel health
habakkuk mcp                        connect to MCP servers and list their status and tools
habakkuk mcp serve                  be an MCP server on stdio (habakkuk_ask, sessions, board, schedules)
habakkuk mcp catalog [QUERY]        list well-known MCP servers Habakkuk can add
habakkuk mcp add NAME [param=value] add a catalog server to config.yaml
habakkuk mcp login [-device] NAME   sign in to a remote MCP server that uses OAuth (auth: oauth)
habakkuk mcp logout NAME            forget that sign-in
habakkuk acp [-agent A]             be a coding agent for an editor (Agent Client Protocol on stdio)
habakkuk send -to CHANNEL:CHAT [-file PATH] TEXT
                                    queue a message or file to a chat without running a turn
habakkuk pair CODE                  let the sender who got this pairing code talk to Habakkuk
habakkuk node pair [-ttl 10m]       print a one-time code (and QR) for pairing a device as a node
habakkuk node run -server URL [-code CODE] -name NAME [-allow-run] [-allow-camera] [-dir DIR]
                                    lend this machine's capabilities to a Habakkuk server
habakkuk nodes [list|revoke NAME]   paired device nodes; revoke cuts one off
habakkuk whatsapp link [-account A] [-phone NUMBER]
                                    link a WhatsApp account (phone gives a code instead of a QR)
habakkuk schedules [list|add|rm|pause|resume|run]
                                    scheduled tasks (add: -agent A -name N -when SPEC -prompt P [-to CHANNEL:CHAT];
                                    scripts: -run CMD [-on-output deliver|agent|silent] [-timeout 2m] [-workdir DIR])
habakkuk board [list|show|add|done|cancel]
                                    the shared task board (add: -agent A -title T [-body B] [-after IDS] [-to CHANNEL:CHAT];
                                    done: ID -result R)
habakkuk meetings [list|show [-transcript] ID|leave ID]
                                    meetings Habakkuk attends (meeting_join): status, notes, transcript
habakkuk sessions [-agent A]        list recent sessions
habakkuk sessions compact SESSION   summarize a long session's older messages now
habakkuk sessions export ID [-format md|json|html] [-o FILE]
                                    write a transcript as Markdown, JSON or an HTML page
habakkuk sessions search [-agent A] QUERY
                                    full-text search over every session
habakkuk sessions archive|unarchive|rm [-y] ID...
                                    hide, restore or delete sessions; sessions archived lists them
habakkuk show SESSION               print a transcript
habakkuk memory [-agent A] [search QUERY | show SLUG | forget SLUG | trust SLUG [yes|no] | profile | log]
habakkuk memory history [-agent A] [-file NAME] [-since 7d] | diff ID | restore ID [-after] | pin|unpin NOTE
                                    list, search, read, remove or vouch for an agent's memories;
                                    print its user profile or the background review log;
                                    list, diff and undo changes to memory, profile, identity
                                    and skills; pin what background jobs must not rewrite
habakkuk retro [-agent A] [-days N] [-dry-run]
                                    look back over an agent's work now: what went wrong, likely
                                    causes, proposed changes (-dry-run prints evidence and plan only)
habakkuk retro list [-agent A] | show ID | apply ID N [N...] | reject ID N
                                    past retros; approve or reject their proposals
habakkuk skills [-agent A]          list the skills an agent can use
habakkuk skills [search|install|update|remove|approve|restore|curate]
                                    skills from GitHub, URLs, paths and registries (habakkuk skills help)
habakkuk plugins [list|search|install|enable|disable|update|remove|restore]
                                    plugins: MCP servers, skills, hooks and commands (docs/tools/plugins.md)
habakkuk checkpoints [-agent A]     list workspace snapshots taken before turns changed files
habakkuk rollback [-agent A] [-y] ID [PATH]
                                    restore the workspace (or one path) to a checkpoint
habakkuk models                     list providers and the models each one serves
habakkuk auth [login|list|logout]   subscription logins (login chatgpt|copilot)
habakkuk usage [-days N] [-agent A] tokens and cost by model and agent, and spend against the limits
habakkuk doctor                     check config, credentials and storage; each problem line ends with its fix
habakkuk status                     version, serve uptime, channels, queue, recent errors, today's cost
habakkuk computer view              how to watch the agents' virtual desktops (computer backend xvfb)
habakkuk logs [-f] [-level warn] [-since 1h] [-grep X] [-n 100]
                                    print habakkuk.log readably (-f keeps following it)
habakkuk security [-fix] [-network] effective tool policy, audit of permissions, exposure and risky
                                    settings (-fix: safe fixes; -network: every outbound endpoint)
habakkuk expose tailscale [-port 443] [-print] | status | off
                                    share the HTTP listener on your tailnet (tailscale serve, never public)
habakkuk remote add NAME TARGET [-identity PATH] [-home DIR] [-bin PATH] [-token-env NAME] [-default]
                                    save a remote: ssh://user@host[:port] runs every command,
                                    https://host runs status and chat (habakkuk remote help)
habakkuk remote [list] | rm NAME | default NAME|none
                                    list, remove, or pick the remote plain commands run on
habakkuk backup [-o FILE] [-env] [-workspaces]
                                    archive config, agents, skills and database snapshots
habakkuk restore [-force] FILE      unpack a backup into $HABAKKUK_HOME
habakkuk update [-check] [-force] [-channel stable|beta]
                                    install the newest release on the channel: checksum (and signature)
                                    verified, checked against this home by the new binary, after a
                                    backup of config and databases
habakkuk update --rollback [-force] put back the previous binary, databases and config
habakkuk config migrate [-dry-run]  rewrite config.yaml in the current format (keeps a timestamped copy)
habakkuk doctor --upgrade-check     check that this binary can run on $HABAKKUK_HOME, changing nothing
habakkuk version                    version, commit, build date and database schema versions
```

Each command is explained in a guide. This table tells you which one:

| Commands | Guide |
|---|---|
| `init`, `setup`, `chat`, `run`, `doctor` | [Quickstart](../get-started/quickstart.md) |
| `chat`, `tui`, `run` | [CLI and TUI](../interfaces/cli-and-tui.md) |
| `talk` | [Voice](../tools/voice.md) |
| `serve`, `setup service` | [Run it on a server](../deploy/server.md) |
| `status`, `logs`, `doctor` | [Observability](../deploy/observability.md) |
| `security` | [Security](../deploy/security.md) |
| `expose` | [Remote access](../deploy/remote-access.md) |
| `remote`, `-r` | [Remote CLI](../interfaces/remote-cli.md) |
| `backup`, `restore` | [Backups and updates](../deploy/backups-and-updates.md) |
| `update`, `config migrate`, `doctor --upgrade-check`, `version` | [Updating](../get-started/updating.md) |
| `migrate` | [Move from another agent](../get-started/migrate.md) |
| `channels`, `pair`, `send` | [Channels](../channels/index.md), [Access, groups and delivery](../channels/settings.md) |
| `whatsapp link` | [WhatsApp](../channels/whatsapp.md#linked-device) |
| `models` | [Providers](../models/providers.md) |
| `auth` | [Subscription logins](../models/subscriptions.md) |
| `usage` | [Usage and limits](../models/usage-and-limits.md) |
| `mcp` | [MCP](../tools/mcp.md) |
| `memory` | [Memory](../agents/memory.md) |
| `retro` | [Retros](../agents/retros.md) |
| `skills` | [Skills](../agents/skills.md), [Plugins](../tools/plugins.md) |
| `plugins` | [Plugins](../tools/plugins.md) |
| `schedules` | [Schedules](../agents/schedules.md) |
| `board` | [Task board](../agents/board.md) |
| `sessions`, `show` | [Sessions](../agents/sessions.md) |
| `checkpoints`, `rollback` | [Files and shell](../tools/files-and-shell.md#checkpoints) |
| `meetings` | [Meetings](../tools/meetings.md) |
| `computer` | [Computer use](../tools/computer-use.md) |
| `acp`, `node`, `nodes` | [Integrations](../tools/integrations.md) |

## Chat commands

You can send these in every chat channel, in the web chat and in `habakkuk chat`. Send `/help` to list them in the chat's language. The terminal UI has a few commands of its own. See [CLI and TUI](../interfaces/cli-and-tui.md#commands).

| Command | What it does |
|---|---|
| `/new` (`/reset`) | Start a fresh conversation |
| `/stop` | Stop the turn in progress |
| `/status` | The agent, session and channel health |
| `/compact` | Summarize the older part of the conversation now |
| `/undo` | Put back the files the latest turn changed; repeat to go further back |
| `/retry` | Run your last message again |
| `/btw <question>` | A side question, answered without adding it to the conversation |
| `/steer <text>` | Guidance for the task in progress, taken in before the next step |
| `/queue <text>` | A message to start on once the current task is done |
| `/think low\|medium\|high\|xhigh\|max\|default` | The reasoning effort for this conversation |
| `/personality <name>\|off` (`/persona`) | Switch persona for this conversation (`concise`, `warm`, `formal`, `technical`, `playful`, or your own) |
| `/goal [-turns N] <text>\|off` | A standing goal worked on turn after turn until done or blocked |
| `/model [provider/model\|default]` | Show or switch the model for this conversation |
| `/usage` | Tokens and cost of this conversation |
| `/budget` | Spending today and this month against the limits |
| `/export` | Send the conversation as a Markdown file |
| `/whoami` | Who you are to the bot, and where you are talking |
| `/agents`, `/agent <name>\|default` | List agents; switch this conversation to another (`/agent default` goes back) |
| `/remember <text>`, `/forget <what>` | Save a memory word for word; remove one |
| `/memory`, `/undo-memory <id>` | The latest changes to memory, profile, identity and skills; undo one |
| `/retro`, `/retro apply N [N...]`, `/retro reject N` | Look back over the agent's last week now; approve or reject the latest retro's proposals (owner only) |
| `/skills`, `/skill NAME [request]`, `/learn <topic, URL or path>` | List skills, run one, or have the agent write a new one |
| `/fork`, `/rewind [N]` | Carry on in a copy of the conversation, optionally without your last N messages |
| `/title [text]` | Show or set the conversation's title |
| `/subagents [stop ID]` (`/tasks`) | List or stop the sub-agent tasks running for this conversation |
| `/progress [plain\|detailed\|off]` | Show or change what this chat shows while the agent works (owner only; no restart needed) |
| `/approvals [summary\|full]` | Show or change what approval requests show in this chat (owner only; no restart needed) |
| `/help` (`/start`) | List the commands |

Plugins can add more commands. See [Plugins](../tools/plugins.md#manifest).
