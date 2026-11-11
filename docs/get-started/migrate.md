---
title: Move from another agent
description: "Bring your agents, memory, past conversations, skills, schedules, MCP servers, channels, keys and your own files over from Hermes Agent or OpenClaw, or your instructions, MCP servers, skills and memory from Claude Code or Codex, with hk migrate."
order: 7
---

If you already run [Hermes Agent](https://github.com/NousResearch/hermes-agent) or [OpenClaw](https://github.com/openclaw/openclaw), this page shows you how to bring your setup over. `hk migrate` reads the other agent's home folder and sets up the same things in Habakkuk. It shows you a plan first, and it writes nothing until you say yes. You can run it again safely, because it leaves alone what's already there.

It also brings your past conversations, so your agent can look back at what you talked about before. It can copy your own files out of the old folder too. That way, the new agent stops depending on the old folder.

Coming from Claude Code or Codex instead? `hk migrate --from claude-code` or `--from codex` brings your instructions, MCP servers, skills, slash commands and memory. See [From Claude Code or Codex](#from-claude-code-or-codex).

```bash
hk migrate --dry-run           # see what would move, change nothing
hk migrate                     # show the plan, ask, then import
hk migrate --files copy        # also bring your own files over from the old folder
hk migrate --history skip      # leave your past conversations behind
```

On a new install, `hk setup` notices any of these homes and offers to import it before it starts its own steps. So running `hk setup` works too.

## What moves

This table shows what the import reads from each agent and what it becomes in Habakkuk.

| From | Hermes Agent | OpenClaw | Becomes in Habakkuk |
|---|---|---|---|
| Agents | the main home and each folder in `profiles/` | each agent in `agents.entries` (or the older `agents.list`) | agents in `config.yaml`; the main one is named `habakkuk` (change it with `--agent`) |
| Persona | `SOUL.md`, `agent.system_prompt` | `SOUL.md`, `AGENTS.md` (with `TOOLS.md` appended), `IDENTITY.md` and the agent's `identity` | `agents/<agent>/SOUL.md`, `AGENTS.md`, `IDENTITY.md` |
| User profile | `memories/USER.md` | `USER.md` | `agents/<agent>/USER.md`, section by section |
| Memory | each entry of `memories/MEMORY.md`, and each fact in `memory_store.db` (tagged with its category) | each `##` section of `MEMORY.md`, and the daily files in `memory/` | one note each in `agents/<agent>/memory/` |
| Past conversations | the sessions in `state.db`, in the main home and each profile | each agent's transcripts in `agents/<id>/sessions/`, or in its `openclaw-agent.sqlite` | read-only sessions of the same agent (see [Your past conversations](#your-past-conversations)) |
| Skills | your own skills in `skills/` (the ones it installed itself are skipped) | `skills/` in each workspace, and the shared `skills/` | `agents/<agent>/skills/`, or the shared `skills/` |
| Schedules | `cron/jobs.json` | the scheduled jobs (`state/openclaw.sqlite`, or the older `cron/jobs.json`) | schedules, **paused** |
| MCP servers | `mcp_servers` | `mcp.servers` | `mcp_servers`, plus an `mcp:<name>` toolset on the agents that used them |
| Models | `model`, `fallback_providers`, `providers` | `agents.defaults.model`, each agent's `model`, `models.providers` | `providers` and each agent's `model` and `fallbacks` |
| Channels | Telegram, Discord, Slack, Matrix, Signal and WhatsApp settings in `.env` | `channels` (with `accounts`) and `bindings` | `channels`, with `allow_from` and routes |
| Keys and tokens | `.env` | `.env`, `env.vars`, values written into the config | `$HABAKKUK_HOME/.env`, under Habakkuk's variable names |
| Your own files | folders and files in the home that the imported text names, such as `workspaces/`, `scripts/` and `state/` | the same; each agent's workspace folder stays where it is | `$HABAKKUK_HOME/files/<name>/`, with `--files copy` (see [Your files in the old folder](#your-files-in-the-old-folder)) |

The import looks for each source folder where that agent's own tools look. That's `~/.hermes` or `$HERMES_HOME`, and `~/.openclaw`, `$OPENCLAW_STATE_DIR` or `~/.openclaw-<profile>` for `$OPENCLAW_PROFILE`. If yours is anywhere else, pass `--source DIR`.

## Your past conversations

Each conversation you had with the old agent comes over as a session of the agent it belonged to. It keeps its title. It starts at its first message and ends at its last. Where the old agent noted the chat, such as a Telegram chat or a Discord thread, the session records that chat under the matching Habakkuk channel.

Only what you and the agent said comes over, each message once. Tool calls, tool results and reasoning stay behind. So do the rows the old agent never showed as part of the chat: the summaries it wrote when it compacted a long chat, notices such as a model switch, group messages it only watched, answers that were rewound or never sent, and text meant for the model alone. When the old agent compacted a chat, it kept a copy of each turn it carried on with, and the import reads that turn once. Context the old gateway added to your messages is taken out too: sender details, forwarded and replied-to messages, the group's chat since the last reply. The gateway puts each of these in a paragraph of its own, with a label line, before or after your words. When a message doesn't split cleanly into those paragraphs and your words, none of it is imported as yours. A message that was only activity the agent watched in a room is left out. Conversations that the old agent, a scheduled task, one of its tools, another bot in a shared room or a remote agent started are marked untrusted, and their prompts are marked as written by that, not by you. A long chat the old agent continued in a new session after compacting it comes over as two sessions, without the turns it carried over counted twice. A conversation with no words in it, only tool calls, is left out.

The import only reads the old agent's databases. It opens them so that nothing is written beside them, not even the `-wal` and `-shm` files SQLite makes. When the old agent has changes in a `-wal` file still, the import reads a copy in a temporary folder and removes it after.

The imported sessions are history:

- Your agent can search them with `session_search`. Results from them are marked as imported, and the agent is told they're reference material, not instructions. A turn whose search results include imported text counts as having read outside content, so its shell commands and writes outside the workspace ask you first. That stays true if the turn is picked up again after a restart. A search that finds only the agent's own chats doesn't count.
- The background memory review never reads them, so nothing in them becomes a memory note or changes your profile on its own. Retros leave them out too: an old user's "no, that's wrong" isn't a correction of your agent.
- A conversation is trusted only when it came from the old agent's terminal or a direct chat in a chat app where a person talks to it (Telegram, Discord, WhatsApp, Slack, Signal, Matrix, SMS, iMessage, Mattermost). Every other one is marked untrusted: group chats, inboxes, webhooks, notification topics such as ntfy, and anything else.
- They show in `hk sessions`, marked `(imported, read-only)`, and on the dashboard's Sessions page. `hk show ID` and `hk sessions export ID` work on them.
- No new message is ever added to them. To pick up an old topic, start a new session and ask the agent about it.
- Retention never prunes them. To remove one, use `hk sessions rm ID`.

The plan shows how many conversations and messages each agent gets, and how many are already there. It counts them without reading them, so the report after the import can differ by a few. Running the import again adds only the ones that are new. A conversation that grew in the old agent after you imported it isn't updated. Pass `--history skip` to leave them all in the old folder.

## From Claude Code or Codex

`hk migrate --from claude-code` and `hk migrate --from codex` set up your main agent from a coding tool. The tool's folder is `~/.claude` or `$CLAUDE_CONFIG_DIR` for Claude Code, and `~/.codex` or `$CODEX_HOME` for Codex. Pass `--source DIR` if yours is somewhere else.

| From | Claude Code | Codex | Becomes in Habakkuk |
|---|---|---|---|
| Instructions | `CLAUDE.md` | `AGENTS.md` (or `AGENTS.override.md`) | the main agent's `AGENTS.md` |
| MCP servers | `mcpServers` in `~/.claude.json`, and Claude Desktop's `claude_desktop_config.json` | `[mcp_servers]` in `config.toml` | `mcp_servers`, plus an `mcp:<name>` toolset on the agent |
| Skills | `skills/`, following linked skill and category folders | `skills/`, and `~/.agents/skills` | shared skills |
| Slash commands | `commands/` (`commands/git/commit.md` is `/git:commit`) | `prompts/` | a shared skill each (`git-commit`), saying what it was and how to use `$ARGUMENTS` |
| Memory | each file in `projects/<project>/memory/` (marked untrusted: Claude Code wrote them from what each project held) | each file in `memories/` (marked untrusted: Codex wrote them itself from past threads) | one note each |
| Model | `model` in `settings.json`, when it's a full model id | `model` and `model_provider` in `config.toml` | the agent's `model` |

Keys and sign-ins never move from a coding tool, not even with `--secrets copy`. The plan names the variables the imported config uses, such as `ANTHROPIC_API_KEY` or a server's token. Set them in `$HABAKKUK_HOME/.env` yourself. Habakkuk can't use a Claude subscription sign-in, so give it an API key with `hk setup model`. If Codex signs in with ChatGPT, the agent gets the ChatGPT sign-in, and you run `hk auth login chatgpt`.

A few MCP servers need a closer look:

- Servers are handled the same way from every source (see [MCP servers and keys](#mcp-servers-and-keys)). From a coding tool, a remote server, or a local one with anything but program names, flags, a package name, paths and variables, is always left out, because no key moves from it. Add it with `hk mcp add NAME`, or in `mcp_servers` in `config.yaml`; a remote server that needs a sign-in takes `auth: oauth` and `hk mcp login NAME`.
- A server that uses the older SSE transport is left out. If it also offers Streamable HTTP, often at `/mcp`, add that URL.
- Codex's `enabled_tools` become the server's `allow` list. Habakkuk can't hide single tools, so its `disabled_tools` always ask for approval instead.
- A server that runs a program inside a desktop app, such as Codex's `node_repl` in the ChatGPT app, is left out.

If `settings.json` names a model by a short name such as `opus`, the import sets no model. Run `hk setup model` first, then `hk migrate --from claude-code` again. Without a model, the agent's MCP servers wait for that second run.

A project folder is read only when you name it with `--project DIR`. Running the import inside one doesn't add it. Here's what a project adds:

- Its instruction files (`CLAUDE.md`, `.claude/CLAUDE.md` and `CLAUDE.local.md`, or Codex's `AGENTS.md`) become a memory note about that project, marked untrusted because they come from a repository. They don't go into the agent's own `AGENTS.md`, which applies everywhere.
- From its `.mcp.json`, only the servers you approved for that folder in Claude Code come along. The import reads that from your own `~/.claude.json`: you accepted the folder's trust prompt there, and then turned on all its servers or that one server, and didn't turn it off. Settings files inside the project don't count, because whoever wrote the repository wrote those. A repository you never opened in Claude Code brings no servers. The plan shows each project server's full command, so you can read what it runs before you say yes. Servers you added for the folder in Claude Code itself come along too.
- Its skills and commands (`.claude/skills` and `.claude/commands`, or `.codex/skills` and `.agents/skills`), only from a folder you trusted: in `~/.claude.json` for Claude Code, or as `trust_level = "trusted"` for that folder in Codex's `config.toml`. Even then they go to the agent's own skills, not the shared ones, and they don't load until you approve each one with `hk skills approve -agent NAME SKILL`. The plan marks them "needs your approval".

The project doesn't become the agent's workspace.

A few things stay behind: past coding sessions, subagents, hooks, plugins, output styles, Codex's command rules and its `notify` command. The plan lists the ones it finds.

When an agent's home and a coding tool are both on your computer, `hk migrate` imports the agent and names the coding tool, so you can run it again with `--from`.

## MCP servers and keys

A key can hide anywhere in a server's setup: a short flag's value, a token in a URL's path or host name, an ID in the query. So the import doesn't guess which values are keys.

A remote server, one with a URL, is never copied as it is. A local server is copied as it is only when its command is a bare program name or a path, and its arguments, apart from paths and variables, are exactly one of these:

- a runner and one package: `npx`, `uvx` or `bunx`, or `pipx run` or `uv tool run`, followed by one package name, such as `@scope/server` or `server@1.2.0` (a version is digits and dots, with an optional `-` pre-release of letters, digits and dots, or `latest`). A runner with no package after it isn't copied as it is;
- nothing at all, for a program that isn't a runner.

The only flags allowed, anywhere, are `-y`, `--yes`, `-q`, `--quiet` and `--stdio`, which take no value. Any other flag, and the word after it, means the server isn't copied as it is. The one exception is a flag whose value is a variable, such as `--token ${NOTION_TOKEN}` or `--token=${NOTION_TOKEN}`: it holds no value, so it stays as it is.

A path here is absolute (or starts with `~/`), has no spaces, line breaks, `@` or `:`, and exists on this computer. A variable is an argument written as `${NAME}`, a flag set to one (`--token=${NAME}`), or any flag followed by one (`--token ${NAME}`): it holds no value, so it stays a variable in `config.yaml`, and its value moves the way keys do. A default, as in `${NAME:-value}`, becomes the variable's value when the old agent had none.

Any other server is left out by default, and the plan says how to add it with `hk mcp add NAME`. With `--secrets copy` (or a yes when you're asked about the keys), the server comes along: each part that isn't plainly a key-free value goes into `$HABAKKUK_HOME/.env` as a variable of its own, such as `MCP_SEARCH_COMMAND`, `MCP_SEARCH_ARG2`, `MCP_SEARCH_URL` (a remote server's whole address) or `MCP_SEARCH_HEADER_1`, and `config.yaml` names the variable. A header written as `${NAME}`, or as `Bearer ${NAME}`, `Basic ${NAME}` or `Token ${NAME}`, stays a variable. A literal value in a server's `env` was always kept as a variable, and moves into `.env` with the other keys. So `config.yaml` and the plan show only program names, flags, package names, paths and variable names; every other value is in `.env`.

From Claude Code and Codex, no key moves, so remote servers and servers with other values are always left out.

## Your files in the old folder

The text you import often names files inside the old agent's folder. For example, a persona file says the briefs are in `~/.hermes/workspaces/briefs`. A schedule runs `~/.hermes/scripts/inbox_sync.py`, and that script keeps its position in `~/.hermes/state/inbox.json`. Leave this as it is and the new agent keeps reading and writing inside the old agent's folder, so you can never remove that folder.

The plan has a section for this, called **Files in ... that the imported text names**. To fill it, the import looks through everything it brings over: persona files, the profile, memory notes, every text file in each skill, schedule commands and prompts, and MCP server commands. It spots the old folder written as a full path (`/home/ada/.hermes/...`), as `~/.hermes/...`, and as `$HOME/.hermes/...` or `${HOME}/.hermes/...`. On Windows it also spots `%USERPROFILE%\.hermes\...`, with either slash and in any letter case. It groups what it finds by the first name under the old folder (`workspaces`, `scripts`, `state`), then sorts each group into one of two kinds:

- **Your own data** can move. That's anything the old agent doesn't keep for itself, such as folders of notes and reports, the scripts your schedules run, and the state files those scripts keep.
- **Parts of the old agent** never move. These are its program and virtual environment, `node_modules`, caches, logs, session and state databases, sockets, lock and process files, its `.env` and sign-in stores, its config, chat logins such as a WhatsApp session, and the skills that came with it. Anything already imported another way (`SOUL.md`, memory files, your skills, schedules) isn't copied a second time. When the text names one of these, the plan lists it with the reason, so you can see what still depends on the old folder.

With `--files copy`, the import copies each of your own entries to `$HABAKKUK_HOME/files/<name>/`. To pick another place, pass `--files-dir DIR`. It then rewrites every reference in the imported text to the new place. A reference written with `~/` or `$HOME` keeps that form when the new place is under your home folder. Text files inside the copies are rewritten too, so a script that names its own state file by full path keeps working. The import also searches the scripts it copies, so a folder that only a copied script names comes along as well.

The copy is careful about what it touches:

- It only reads the old folder. Nothing in there is changed or removed.
- It keeps file modes, modification times and symlinks. A link is copied as a link and isn't followed, so nothing outside the old folder gets pulled in. A link to something that moves points at the copy.
- If there's a git repository inside your files, it's copied whole.
- It leaves out sockets, pipes and devices. It also leaves out any file over 1 GB, unless you raise the limit with `--files-max`. The plan names everything that's left out.
- It leaves out a file the old agent keeps for itself inside one of your folders, too. Hermes Agent's gateway keeps one in `state/`, next to the state files of your scripts.
- You can stop it and run it again. It copies a file only when the file is missing from the copy or newer in the old folder. It never deletes anything in the copy. If a file has changed in the copy since, that version is kept, because the new agent works there now.
- It checks the free space first. If there isn't enough, it refuses to start.
- It rewrites references only in text files up to 1 MB. Larger files and binary files are copied byte for byte.

A Hermes Agent profile is a home of its own inside `profiles/`. Its folders keep the profile in their name, so `profiles/coder/scripts` goes to `files/profiles/coder/scripts`.

Each OpenClaw agent's workspace folder is already handled. Habakkuk uses it where it is, and `config.yaml` points at it, so it isn't copied again. The plan lists it under what is not moved.

`hk backup` doesn't include `files/`. Back that folder up the same way you back up your other documents.

## What does not move

| What | Why | Instead |
|---|---|---|
| The old agent's own folders (program, caches, logs, sessions, databases, keys, config) | They belong to the old agent, not to you | They stay in the old folder; the plan lists the ones your text still names |
| Tool calls, tool results and reasoning in past conversations | Only what you and the agent said is imported | They stay in the old folder |
| The WhatsApp linked device | The session belongs to a different WhatsApp library | `hk whatsapp link` (or `hk setup channel whatsapp`) links the number again |
| Subscription and OAuth sign-ins | Tokens are tied to the app that signed in | `hk auth login chatgpt` or `copilot`, `hk mcp login NAME`, or an API key with `hk setup model` |
| Keys kept in OpenClaw's credential store | They are not in `.env` or the config | Put them in `$HABAKKUK_HOME/.env`; the plan names the variables it still needs |
| Matrix encryption keys | Device keys cannot be copied | Verify the new session from another Matrix client |
| Board cards, plugins, hooks, `HEARTBEAT.md` | No direct equivalent | `hk board add`, hk plugins, `hooks` in `config.yaml`, `hk schedules add` |
| Other channels (email, iMessage, ...) | Not mapped yet | `hk setup channel`, or the channels guide |

A few more things are left out: schedules and MCP servers that are turned off, one-off schedules whose time has passed, and skills that came with the other agent. The plan lists them at the end.

## The command

```text
hk migrate [--from hermes|openclaw|claude-code|codex] [--source DIR]
                 [--agent NAME] [--project DIR] [--dry-run]
                 [--secrets copy|skip] [--history copy|skip]
                 [--files copy|skip] [--files-dir DIR] [--files-max SIZE]
                 [--force] [-y]
```

| Flag | Meaning |
|---|---|
| `--from` | `hermes`, `openclaw`, `claude-code` or `codex`. Without it, the one found on this computer is used. An agent found beside a coding tool is picked, and the coding tool is named. If two agents are there, you must choose. |
| `--source DIR` | The source home folder, when it is not in the default place. |
| `--agent NAME` | What the main agent is called in Habakkuk (default `habakkuk`). Other agents keep their names. |
| `--project DIR` | Claude Code and Codex only: also bring this project folder's instructions (as a memory note), the MCP servers you approved for it, and its skills. Without it, no project folder is read. |
| `--dry-run` | Print the plan and stop. Nothing is written, not even an empty folder. It shows the files that would be copied, their sizes, and what would stay. |
| `--secrets copy` | Copy keys and tokens into `$HABAKKUK_HOME/.env` (readable only by you). |
| `--secrets skip` | Copy no keys; the report lists the variables to set yourself. |
| `--history copy` | Import your past conversations as read-only sessions. This is the default. |
| `--history skip` | Leave your past conversations in the old folder. |
| `--files copy` | Copy your own files that the imported text names into `$HABAKKUK_HOME/files`, and rewrite the text to name the copies. |
| `--files skip` | Copy no files. The imported text keeps naming the old folder, and the report lists what it names. |
| `--files-dir DIR` | Where `--files copy` puts the files, instead of `$HABAKKUK_HOME/files`. It is remembered: later runs keep using the place where each folder went. |
| `--files-max SIZE` | The largest file `--files copy` takes, such as `500M` or `4G`. The default is `1G`; `0` means no limit. |
| `--force` | Replace agents, persona files, skills, notes, schedules and `.env` values that already exist and differ. |
| `-y` | Do not ask. Keys are copied only with `--secrets copy`, and files only with `--files copy`. |

If you leave out `--secrets`, an interactive run asks once whether to copy the keys. If you leave out `--files`, it asks once whether to copy your files. With `-y` and no `--files`, your files stay where they are. The report tells you how to bring them over later, which is to run `hk migrate --files copy`. That later run also fixes what was imported before. It rewrites in place any text that still names the old folder, and each schedule keeps its ID and stays on or paused. Text you changed since the import is covered too. Your wording is kept, and only the paths into the copied folders change.

> [!NOTE]
> You only ever see keys by name. Their values go straight into `.env`. They don't appear in `config.yaml`, the plan, the report or the logs. A memory entry that looks like it holds a key or password isn't imported. An MCP server whose values might be keys comes along only with the keys, as variables in `.env` (see [MCP servers and keys](#mcp-servers-and-keys)), and the plan shows URLs without their query. From Claude Code and Codex no key is copied at all.

## A worked example

This example starts from a Hermes home with one profile, `coder`:

```console
$ hk migrate --dry-run
Found Hermes Agent at /home/ada/.hermes.
Importing into /home/ada/.habakkuk. + new, = already there, ~ added to, ! conflict (left alone), * replaced (--force), x not imported.

Agents
  + habakkuk (was default): openrouter/anthropic/claude-sonnet-4, workspace ~/habakkuk-workspace
  + coder: anthropic/claude-opus-4-6

Persona and profile
  + habakkuk: AGENTS.md
  + habakkuk: SOUL.md
  + habakkuk: USER.md
  + coder: SOUL.md

Memory notes
  + habakkuk: 3 to import, 0 already there
  x habakkuk: "Backup token for the nas": it looks like it holds a key or password

Skills
  + notes-helper: shared
  x risky: the security scan flagged install.sh:2: pipe-to-shell: curl ... | sh; read it, then hk skills install ...
  + go-review: agent coder

Schedules (imported paused)
  + habakkuk: Morning brief: CRON_TZ=America/Toronto 0 8 * * 1-5
  + habakkuk: Inbox sweep: @every 30m

MCP servers
  + github: npx
  + notion: https://mcp.notion.example/mcp

Model providers
  + anthropic: preset anthropic
  + openrouter: preset openrouter

Channels
  + telegram: telegram for agent habakkuk
  + whatsapp: whatsapp for agent habakkuk

Conversation history (imported read-only)
  + habakkuk: 38 conversations (1204 messages) to import, 0 already there
  + coder: 3 conversations (57 messages) to import, 0 already there

Files in /home/ada/.hermes that the imported text names
  + scripts: 12 KB in 3 files, named 3 times -> /home/ada/.habakkuk/files/scripts
  + state: 4 KB in 1 file, named 2 times -> /home/ada/.habakkuk/files/state
  + workspaces: 2.5 GB in 1204 files, named 6 times -> /home/ada/.habakkuk/files/workspaces
  Inside the copied files, 3 references in 2 files will name the copies too.
  Not moved (/home/ada/.hermes is still needed for these):
  - .env: named 2 times (scripts/inbox_sync.py, scripts/report.sh): Hermes Agent's keys and sign-ins (the keys the import uses go to Habakkuk's .env)
  - config.yaml: named once (habakkuk: SOUL.md): Hermes Agent's config (its settings are imported)
  - hermes-agent: named once (scripts/report.sh): Hermes Agent's own program
  You will be asked whether to copy them (2.5 GB) into /home/ada/.habakkuk/files. --files copy or --files skip answers ahead.

Keys and tokens (names only; values are never shown)
  + ANTHROPIC_API_KEY
  + GITHUB_PERSONAL_ACCESS_TOKEN
  + NOTION_TOKEN
  + OPENROUTER_API_KEY
  + TELEGRAM_BOT_TOKEN

Cannot carry over
  - the WhatsApp linked-device session: hk whatsapp link (or hk setup channel whatsapp) to link the number again

config.yaml will be created.

Dry run: nothing was written.
```

Next, run the import, and copy the keys and your files with it:

```console
$ hk migrate --secrets copy --files copy -y
...
Copying 2.5 GB into /home/ada/.habakkuk/files ...

Imported 2 agents, 3 persona files, 1 profile, 3 memory notes, 2 skills, 2 schedules, 2 MCP servers, 2 providers, 2 channels, 41 past conversations (1261 messages) as read-only sessions.
Copied ANTHROPIC_API_KEY, GITHUB_PERSONAL_ACCESS_TOKEN, NOTION_TOKEN, OPENROUTER_API_KEY, TELEGRAM_BOT_TOKEN into /home/ada/.habakkuk/.env.

Copied 1208 files (2.5 GB) out of /home/ada/.hermes into /home/ada/.habakkuk/files: scripts, state, workspaces.
Rewrote 11 references to name the copies: 3 in persona files, 2 in skills, 3 in schedules, 3 inside 2 copied files.
Still points into /home/ada/.hermes (that folder is needed while these remain):
  - .env: named 2 times (scripts/inbox_sync.py, scripts/report.sh): Hermes Agent's keys and sign-ins (the keys the import uses go to Habakkuk's .env)
  - config.yaml: named once (habakkuk: SOUL.md): Hermes Agent's config (its settings are imported)
  - hermes-agent: named once (scripts/report.sh): Hermes Agent's own program

Schedules are imported paused. Check them with hk schedules list, then resume each one:
  hk schedules resume sch_6f9580dd273695f7          # habakkuk/Inbox sweep
  hk schedules resume sch_4066676d3e5680b4          # habakkuk/Morning brief

Next:
  hk doctor            check the config, keys and channels
  hk serve             start the agents and connect the channels
  hk migrate --dry-run             see what still points into /home/ada/.hermes before you remove it
```

After this run, `SOUL.md` says the briefs are in `~/.habakkuk/files/workspaces/briefs`. The schedule runs `/home/ada/.habakkuk/files/scripts/inbox_sync.py`, and that script reads `/home/ada/.habakkuk/files/state/inbox.json`. The three things under "Still points into" are what's left for you to do by hand. In this example, `report.sh` loads the old `.env` and calls the old agent's own command. You need to edit that one script before the old folder can go.

> [!WARNING]
> Stop the old agent before `hk serve` starts the same channels. Only one program at a time can poll a Telegram, Discord or Slack bot. And if two agents answer one chat, everyone gets confused.

Schedules come in paused, so nothing runs twice while both agents are installed. The usual time limit for a script schedule is two minutes. An imported one gets ten minutes (or its interval, when an `@every` interval is shorter), because the old agent let scripts run longer. Once the old agent is stopped, resume each schedule. Without `--files copy`, a script schedule still runs the script from the old folder. With it, the script is copied and the schedule runs the copy.

After the import, read through each agent's `SOUL.md` and `AGENTS.md`. Instructions written for the old agent may name its tools or commands, and Habakkuk doesn't have those.

## Running it again

When you run the import again, it compares everything with what's already in `$HABAKKUK_HOME`. Each item falls into one of three cases:

- **Already there** (`=`): the same file, note, skill, schedule, server or channel already exists, so nothing happens.
- **Conflict** (`!`): something different exists under the same name, for example a `SOUL.md` you wrote. The import leaves it alone and lists it. Pass `--force` to replace it. One thing in a conflict does change when files are copied. If it has a path that leads into a copied folder, that path is rewritten to the copy, and the plan says so on that line. Everything else in it stays as you wrote it. Replaced files are kept as `NAME.bak`, and replaced skills as `<skill>.bak`. `config.yaml` is always kept as `config.yaml.bak` before it's edited.
- **Added to** (`~`): the import only ever adds to the user profile and to an agent's toolsets. It doesn't cut anything from them.

Past conversations are matched by their id in the old agent, so each one comes over once. A conversation you deleted from Habakkuk comes back on the next run. Pass `--history skip` if you don't want that.

The import edits `config.yaml` in place, the same way `hk setup` does, so your comments and settings stay.

Copied files work the same way. A second `--files copy` brings over only what's new or changed in the old folder, and it shows `=` for a folder with nothing to copy. It doesn't delete from the copy, and it doesn't replace a file that changed there since. The import keeps a small record of which folder went where, in `migrated-files.json` in `$HABAKKUK_HOME`. Later runs read that record, with or without `--files`. That's how text that already names the copies is seen as already there.

## Troubleshooting

**The old agent's folder is still needed**: run `hk migrate --dry-run` and read the Files section. The entries marked `-` under "Not moved" are the things the imported text and the copied scripts still name there. If the list shows your own folders as not copied, run `hk migrate --files copy`. Whatever remains after that is part of the old agent. You need to decide what to do about each one:

- its `.env`: the keys the import uses are already in `$HABAKKUK_HOME/.env`. Point the script at that file, or set the other variables there.
- its program, for example when a script calls the old agent's own command: change the script to use `hk run` or `hk send`, or keep the old agent installed.
- its config, logs or session database, named in a persona file or note: edit the text, because Habakkuk has its own.
- "(the folder itself)": the text names the old folder as a whole. Read that text and decide what it should mean now.

Once the Files section lists nothing under "Not moved" and every folder shows `=`, nothing you imported names the old folder any more. Before you remove it, check that the old agent is stopped and that your schedules have run once from the new place.

**"not enough room in ..."**: the copy needs more free space than the disk has. You can free some space, pass `--files-dir` with a folder on a bigger disk, or use `--files skip` and move the files yourself.

**A file was left out as too large**: raise the limit and run `hk migrate --files copy` again. For example, pass `--files-max 8G`, or `--files-max 0` for no limit. The second run copies only the missing files.

**A reference was not rewritten**: the import finds only paths that start with the old folder. It doesn't change a script that builds the path from pieces, or reads it from a variable of the old agent. A folder name with a space is followed when the text writes it plainly or in quotes (`"~/.hermes/My Notes/todo.md"`). It isn't followed when the space is escaped (`My\ Notes`). The plan lists that one under "Not moved" and asks you to edit the text. Search your copied scripts for the old folder's name, and fix what's left by hand.

**"found both Hermes Agent and OpenClaw"**: pass `--from hermes` or `--from openclaw`. If you want both, run it again for the other one.

**"found no setup to import"**: the home is somewhere else, so pass `--from` and `--source DIR`.

**"Found Claude Code too"** (or Codex): the import picked the agent's home. Run `hk migrate --from claude-code` (or `--from codex`) after it to bring the coding tool's setup into the same agent.

**An MCP server shows "values in its command line, URL or headers may be keys"**: the import couldn't tell its values from keys. Run the import again with `--secrets copy` to bring it with those values in `.env`. Or add it yourself with `hk mcp add NAME`, or in `mcp_servers` in `config.yaml`, where an argument or the URL can name a variable as `${NAME}`.

**A project's MCP server shows "you never approved it for this folder in Claude Code"**: your `~/.claude.json` doesn't say you trusted it. Read its command in `.mcp.json`. If you trust it, open the folder in Claude Code, approve it, and run the import again, or add it to `config.yaml` yourself.

**"this conversation was imported from another agent and is read-only"**: you sent a message to an imported session. Start a new session instead. The agent can still find the old conversation with `session_search`.

**"config.yaml does not parse as YAML" or "openclaw.json does not parse as JSON5"**: the source config is damaged. The message says where. Fix it, then run the import again.

**"... does not load, so the import cannot edit it safely"**: there's an error in your Habakkuk `config.yaml`. Run `hk doctor`, fix the error, and run the import again.

**An agent shows "no model found"**: its provider has no Habakkuk equivalent, or it used a subscription sign-in. Its files are imported anyway. Give it a model with `hk setup model -agent NAME`, then run `hk migrate` again to add its channels.

**A skill shows "the security scan flagged ..."**: the import runs the same scan as `hk skills install`. Read the flagged file. If you trust it, install it with `hk skills install PATH`, adding `-force` for high-risk findings.

**A schedule's delivery was dropped**: the schedule sent to a channel that wasn't imported. Add the channel, then create the schedule again with `hk schedules add ... -to CHANNEL:CHAT`.

**Variables "the config names but ... held no value for"**: these were set in your shell and weren't in the old `.env`. Add them to `$HABAKKUK_HOME/.env`.
