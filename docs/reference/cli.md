---
title: CLI
description: Every habakkuk command and its flags, and the slash commands available in a chat.
order: 1
---

`habakkuk` with no arguments prints this list. `HABAKKUK_HOME` (default `~/.habakkuk`) selects the home every command works on.

## Commands

```text
habakkuk init                       write a starter config to $HABAKKUK_HOME (default ~/.habakkuk)
habakkuk setup                      interactive setup: provider, API key, model, workspace, Telegram
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
                                    list, search, read, remove or vouch for an agent's memories;
                                    print its user profile or the background review log
habakkuk skills [-agent A]          list the skills an agent can use
habakkuk skills [search|install|update|remove|approve|restore|curate]
                                    skills from GitHub, URLs, paths and registries (habakkuk skills help)
habakkuk plugins [list|search|install|enable|disable|update|remove|restore]
                                    plugins: MCP servers, skills, hooks and commands
habakkuk checkpoints [-agent A]     list workspace snapshots taken before turns changed files
habakkuk rollback [-agent A] [-y] ID [PATH]
                                    restore the workspace (or one path) to a checkpoint
habakkuk models                     list providers and the models each one serves
habakkuk auth [login|list|logout]   subscription logins (login chatgpt|copilot)
habakkuk usage [-days N] [-agent A] tokens and cost by model and agent
habakkuk doctor                     check config, credentials and storage
habakkuk status                     version, serve uptime, channels, queue, recent errors, today's cost
habakkuk computer view              how to watch the agents' virtual desktops (computer backend xvfb)
habakkuk logs [-f] [-level warn] [-since 1h] [-grep X] [-n 100]
                                    print habakkuk.log readably (-f keeps following it)
habakkuk security audit [-fix]      check permissions, exposure and risky settings (-fix: safe fixes)
habakkuk expose tailscale [-port 443] [-print] | status | off
                                    share the HTTP listener on your tailnet (tailscale serve, never public)
habakkuk backup [-o FILE] [-env] [-workspaces]
                                    archive config, agents, skills and database snapshots
habakkuk restore [-force] FILE      unpack a backup into $HABAKKUK_HOME
habakkuk update [-check] [-force]   install the latest release (checksum verified)
habakkuk version
```

Where each command is explained:

| Commands | Guide |
|---|---|
| `init`, `setup`, `chat`, `run`, `doctor` | [Quick start](../getting-started/quickstart.md) |
| `tui`, `talk` | [Quick start](../getting-started/quickstart.md), [Voice](../guides/voice.md) |
| `serve`, `status`, `logs`, `security`, `expose`, `backup`, `restore`, `update` | [Deployment](../guides/deployment.md) |
| `channels`, `pair`, `send`, `whatsapp` | [Channels](../guides/channels.md), [WhatsApp](../guides/whatsapp.md) |
| `models`, `auth`, `usage` | [Models and providers](../guides/models.md) |
| `mcp` | [MCP servers](../guides/mcp.md) |
| `skills`, `plugins`, `memory` | [Memory and skills](../guides/memory-and-skills.md), [Plugins](../guides/plugins.md) |
| `schedules`, `board` | [Schedules and the task board](../guides/schedules-and-board.md) |
| `sessions`, `show` | [Agents](../guides/agents.md#sessions) |
| `checkpoints`, `rollback` | [Tools and approvals](../guides/tools-and-approvals.md#checkpoints) |
| `meetings` | [Meetings](../guides/meetings.md) |
| `computer` | [Browser and computer use](../guides/browser-and-computer.md#computer) |
| `acp`, `node`, `nodes` | [Integrations](../guides/integrations.md) |

## Chat commands

These work in every chat channel, the web chat and `habakkuk chat`. `/help` lists them in the chat's language.

| Command | What it does |
|---|---|
| `/new` | Start a fresh conversation |
| `/stop` | Stop the turn in progress |
| `/status` | The agent, session and channel health |
| `/compact` | Summarize the older part of the conversation now |
| `/undo` | Put back the files the latest turn changed; repeat to go further back |
| `/retry` | Run your last message again |
| `/btw <question>` | A side question, answered without adding it to the conversation |
| `/steer <text>` | Guidance for the task in progress, taken in before the next step |
| `/queue <text>` | A message to start on once the current task is done |
| `/think low\|medium\|high\|xhigh\|max` | The reasoning effort for this conversation |
| `/personality <name>` | Switch persona for this conversation (`concise`, `warm`, `formal`, `technical`, `playful`, or your own) |
| `/goal [-turns N] <text>` | A standing goal worked on turn after turn until done or blocked |
| `/model [provider/model\|default]` | Show or switch the model for this conversation |
| `/usage` | Tokens and cost of this conversation |
| `/export` | Send the conversation as a Markdown file |
| `/whoami` | Who you are to the bot, and where you are talking |
| `/agents`, `/agent <name>` | List agents; switch this conversation to another (`/agent default` goes back) |
| `/remember <text>`, `/forget <what>` | Save a memory word for word; remove one |
| `/skills`, `/skill NAME [request]`, `/learn <topic, URL or path>` | List skills, run one, or have the agent write a new one |
| `/fork`, `/rewind [N]` | Carry on in a copy of the conversation, optionally without the last N messages |
| `/title [text]` | Show or set the conversation's title |
| `/subagents [stop ID]` | List or stop the sub-agent tasks running for this conversation |

Plugins can add more commands; see [Plugins](../guides/plugins.md).
