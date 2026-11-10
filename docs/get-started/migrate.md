---
title: Move from another agent
description: "Bring your agents, memory, skills, schedules, MCP servers, channels, keys and your own files over from Hermes Agent or OpenClaw with habakkuk migrate."
order: 7
---

If you already run [Hermes Agent](https://github.com/NousResearch/hermes-agent) or [OpenClaw](https://github.com/openclaw/openclaw), `habakkuk migrate` reads its home folder and sets up the same things in Habakkuk. It shows you a plan first and writes nothing until you say yes. Running it again is safe: what is already there is left alone.

It can also copy your own files out of the old folder, so the new agent stops depending on it.

```bash
habakkuk migrate --dry-run     # see what would move, change nothing
habakkuk migrate               # show the plan, ask, then import
habakkuk migrate --files copy  # also bring your own files over from the old folder
```

On a new install, `habakkuk setup` notices either home and offers to import it before its own steps, so you can also just run `habakkuk setup`.

## What moves

| From | Hermes Agent | OpenClaw | Becomes in Habakkuk |
|---|---|---|---|
| Agents | the main home and each folder in `profiles/` | each agent in `agents.entries` (or the older `agents.list`) | agents in `config.yaml`; the main one is named `habakkuk` (change it with `--agent`) |
| Persona | `SOUL.md`, `agent.system_prompt` | `SOUL.md`, `AGENTS.md` (with `TOOLS.md` appended), `IDENTITY.md` and the agent's `identity` | `agents/<agent>/SOUL.md`, `AGENTS.md`, `IDENTITY.md` |
| User profile | `memories/USER.md` | `USER.md` | `agents/<agent>/USER.md`, section by section |
| Memory | each entry of `memories/MEMORY.md`, and each fact in `memory_store.db` (tagged with its category) | each `##` section of `MEMORY.md`, and the daily files in `memory/` | one note each in `agents/<agent>/memory/` |
| Skills | your own skills in `skills/` (the ones it installed itself are skipped) | `skills/` in each workspace, and the shared `skills/` | `agents/<agent>/skills/`, or the shared `skills/` |
| Schedules | `cron/jobs.json` | the scheduled jobs (`state/openclaw.sqlite`, or the older `cron/jobs.json`) | schedules, **paused** |
| MCP servers | `mcp_servers` | `mcp.servers` | `mcp_servers`, plus an `mcp:<name>` toolset on the agents that used them |
| Models | `model`, `fallback_providers`, `providers` | `agents.defaults.model`, each agent's `model`, `models.providers` | `providers` and each agent's `model` and `fallbacks` |
| Channels | Telegram, Discord, Slack, Matrix, Signal and WhatsApp settings in `.env` | `channels` (with `accounts`) and `bindings` | `channels`, with `allow_from` and routes |
| Keys and tokens | `.env` | `.env`, `env.vars`, values written into the config | `$HABAKKUK_HOME/.env`, under Habakkuk's variable names |
| Your own files | folders and files in the home that the imported text names, such as `workspaces/`, `scripts/` and `state/` | the same; each agent's workspace folder stays where it is | `$HABAKKUK_HOME/files/<name>/`, with `--files copy` (see [Your files in the old folder](#your-files-in-the-old-folder)) |

Source folders are found the way their own tools find them: `~/.hermes` or `$HERMES_HOME`, and `~/.openclaw`, `$OPENCLAW_STATE_DIR` or `~/.openclaw-<profile>` for `$OPENCLAW_PROFILE`. Pass `--source DIR` for anywhere else.

## Your files in the old folder

The text you import often names files inside the old agent's folder. A persona file says the briefs are in `~/.hermes/workspaces/briefs`. A schedule runs `~/.hermes/scripts/inbox_sync.py`. That script keeps its position in `~/.hermes/state/inbox.json`. If nothing is done about this, the new agent keeps reading and writing inside the old agent's folder, and you can never remove it.

The plan has a section for this, **Files in ... that the imported text names**. The import looks through everything it brings over: persona files, the profile, memory notes, every text file in each skill, schedule commands and prompts, and MCP server commands. It finds the old folder written as a full path (`/home/ada/.hermes/...`), as `~/.hermes/...`, and as `$HOME/.hermes/...` or `${HOME}/.hermes/...`. On Windows it also finds `%USERPROFILE%\.hermes\...`, with either slash and in any letter case. It groups what it finds by the first name under the old folder (`workspaces`, `scripts`, `state`) and sorts each one:

- **Your own data** can move. Anything the old agent does not keep for itself counts: folders of notes and reports, the scripts your schedules run, the state files those scripts keep.
- **Parts of the old agent** never move: its program and virtual environment, `node_modules`, caches, logs, session and state databases, sockets, lock and process files, its `.env` and sign-in stores, its config, chat logins such as a WhatsApp session, and the skills that came with it. What is already imported another way (`SOUL.md`, memory files, your skills, schedules) is not copied a second time. The plan lists each of these that the text names, with the reason, so you can see what still depends on the old folder.

With `--files copy`, each of your own entries is copied to `$HABAKKUK_HOME/files/<name>/` (`--files-dir DIR` picks another place), and every reference in the imported text is rewritten to the new place. A reference written with `~/` or `$HOME` keeps that form when the new place is under your home folder. Text files inside the copies are rewritten too, so a script that names its own state file by full path keeps working. Scripts that are copied are searched as well: a folder that only a copied script names comes along.

The copy is careful:

- The old folder is only read. Nothing in it is changed or removed.
- File modes, modification times and symlinks are kept. A link is copied as a link and never followed, so nothing outside the old folder is pulled in. A link to something that moves points at the copy.
- A git repository inside your files is copied whole.
- Sockets, pipes and devices are left out. So is any file over 1 GB, unless you raise the limit with `--files-max`. The plan names what is left out.
- It can be stopped and run again. A file is copied only when it is missing in the copy or newer in the old folder. Nothing in the copy is ever deleted. A file that changed in the copy since is kept, because the new agent works there now.
- It checks the free space first and refuses to start when there is not enough.
- Only text files up to 1 MB have their references rewritten. Larger files and binary files are copied byte for byte.

A Hermes Agent profile is a home of its own inside `profiles/`. Its folders are named with it: `profiles/coder/scripts` goes to `files/profiles/coder/scripts`.

Each OpenClaw agent's workspace folder is already handled: Habakkuk uses it where it is, and `config.yaml` points at it. It is not copied again. The plan lists it under what is not moved.

`habakkuk backup` does not include `files/`. Back that folder up the way you back up your other documents.

## What does not move

| What | Why | Instead |
|---|---|---|
| The old agent's own folders (program, caches, logs, sessions, databases, keys, config) | They belong to the old agent, not to you | They stay in the old folder; the plan lists the ones your text still names |
| Conversation history | Each agent stores sessions in its own database format | It stays in the old folder; Habakkuk starts fresh sessions |
| The WhatsApp linked device | The session belongs to a different WhatsApp library | `habakkuk whatsapp link` (or `habakkuk setup channel whatsapp`) links the number again |
| Subscription and OAuth sign-ins | Tokens are tied to the app that signed in | `habakkuk auth login chatgpt` or `copilot`, `habakkuk mcp login NAME`, or an API key with `habakkuk setup model` |
| Keys kept in OpenClaw's credential store | They are not in `.env` or the config | Put them in `$HABAKKUK_HOME/.env`; the plan names the variables it still needs |
| Matrix encryption keys | Device keys cannot be copied | Verify the new session from another Matrix client |
| Board cards, plugins, hooks, `HEARTBEAT.md` | No direct equivalent | `habakkuk board add`, habakkuk plugins, `hooks` in `config.yaml`, `habakkuk schedules add` |
| Other channels (email, iMessage, ...) | Not mapped yet | `habakkuk setup channel`, or the channels guide |

Turned-off schedules and MCP servers, one-off schedules whose time has passed, and skills that came with the other agent are left out and listed at the end of the plan.

## The command

```text
habakkuk migrate [--from hermes|openclaw] [--source DIR] [--agent NAME]
                 [--dry-run] [--secrets copy|skip] [--files copy|skip]
                 [--files-dir DIR] [--files-max SIZE] [--force] [-y]
```

| Flag | Meaning |
|---|---|
| `--from` | `hermes` or `openclaw`. Without it, the one found on this computer is used; if both are there you must choose. |
| `--source DIR` | The source home folder, when it is not in the default place. |
| `--agent NAME` | What the main agent is called in Habakkuk (default `habakkuk`). Other agents keep their names. |
| `--dry-run` | Print the plan and stop. Nothing is written, not even an empty folder. It shows the files that would be copied, their sizes, and what would stay. |
| `--secrets copy` | Copy keys and tokens into `$HABAKKUK_HOME/.env` (readable only by you). |
| `--secrets skip` | Copy no keys; the report lists the variables to set yourself. |
| `--files copy` | Copy your own files that the imported text names into `$HABAKKUK_HOME/files`, and rewrite the text to name the copies. |
| `--files skip` | Copy no files. The imported text keeps naming the old folder, and the report lists what it names. |
| `--files-dir DIR` | Where `--files copy` puts the files, instead of `$HABAKKUK_HOME/files`. It is remembered: later runs keep using the place where each folder went. |
| `--files-max SIZE` | The largest file `--files copy` takes, such as `500M` or `4G`. The default is `1G`; `0` means no limit. |
| `--force` | Replace agents, persona files, skills, notes, schedules and `.env` values that already exist and differ. |
| `-y` | Do not ask. Keys are copied only with `--secrets copy`, and files only with `--files copy`. |

Without `--secrets`, an interactive run asks once whether to copy the keys. Without `--files`, it asks once whether to copy your files. With `-y` and no `--files`, the files stay where they are and the report tells you how to bring them over later: run `habakkuk migrate --files copy`. That later run also fixes what was imported before. Text that still names the old folder is rewritten in place, and a schedule keeps its ID and stays on or paused.

> [!NOTE]
> Keys are only ever shown by name. Their values go straight into `.env` and never into `config.yaml`, the plan, the report or the logs. A memory entry that looks like it holds a key or password is not imported.

## A worked example

A Hermes home with one profile, `coder`:

```console
$ habakkuk migrate --dry-run
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
  x risky: the security scan flagged install.sh:2: pipe-to-shell: curl ... | sh; read it, then habakkuk skills install ...
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
  - conversation history: it stays in /home/ada/.hermes; Habakkuk starts fresh sessions
  - the WhatsApp linked-device session: habakkuk whatsapp link (or habakkuk setup channel whatsapp) to link the number again

config.yaml will be created.

Dry run: nothing was written.
```

Then import, copying the keys and your files:

```console
$ habakkuk migrate --secrets copy --files copy -y
...
Copying 2.5 GB into /home/ada/.habakkuk/files ...

Imported 2 agents, 3 persona files, 1 profile, 3 memory notes, 2 skills, 2 schedules, 2 MCP servers, 2 providers, 2 channels.
Copied ANTHROPIC_API_KEY, GITHUB_PERSONAL_ACCESS_TOKEN, NOTION_TOKEN, OPENROUTER_API_KEY, TELEGRAM_BOT_TOKEN into /home/ada/.habakkuk/.env.

Copied 1208 files (2.5 GB) out of /home/ada/.hermes into /home/ada/.habakkuk/files: scripts, state, workspaces.
Rewrote 11 references to name the copies: 3 in persona files, 2 in skills, 3 in schedules, 3 inside 2 copied files.
Still points into /home/ada/.hermes (that folder is needed while these remain):
  - .env: named 2 times (scripts/inbox_sync.py, scripts/report.sh): Hermes Agent's keys and sign-ins (the keys the import uses go to Habakkuk's .env)
  - config.yaml: named once (habakkuk: SOUL.md): Hermes Agent's config (its settings are imported)
  - hermes-agent: named once (scripts/report.sh): Hermes Agent's own program

Schedules are imported paused. Check them with habakkuk schedules list, then resume each one:
  habakkuk schedules resume sch_6f9580dd273695f7    # habakkuk/Inbox sweep
  habakkuk schedules resume sch_4066676d3e5680b4    # habakkuk/Morning brief

Next:
  habakkuk doctor      check the config, keys and channels
  habakkuk serve       start the agents and connect the channels
  habakkuk migrate --dry-run       see what still points into /home/ada/.hermes before you remove it
```

After this, `SOUL.md` says the briefs are in `~/.habakkuk/files/workspaces/briefs`, the schedule runs `/home/ada/.habakkuk/files/scripts/inbox_sync.py`, and that script reads `/home/ada/.habakkuk/files/state/inbox.json`. The three things under "Still points into" are what is left to do by hand. Here `report.sh` loads the old `.env` and calls the old agent's own command, so that one script needs editing before the old folder can go.

> [!WARNING]
> Stop the old agent before `habakkuk serve` starts the same channels. A Telegram, Discord or Slack bot can only be polled by one program at a time, and two agents answering one chat confuses everyone.

Schedules are imported paused so nothing runs twice while both agents are installed. A script schedule gets a ten-minute time limit (or its interval, when an `@every` interval is shorter) instead of the usual two minutes, because the old agent let scripts run longer. Resume each one once the old agent is stopped. Without `--files copy`, a script schedule still runs the script from the old folder. With it, the script is copied and the schedule runs the copy.

After the import, read each agent's `SOUL.md` and `AGENTS.md`: instructions written for the old agent may name its tools or commands, which Habakkuk does not have.

## Running it again

The import compares everything with what is already in `$HABAKKUK_HOME`:

- **Already there** (`=`): the same file, note, skill, schedule, server or channel exists. Nothing happens.
- **Conflict** (`!`): something different exists under the same name, for example a `SOUL.md` you wrote. It is left alone and listed; `--force` replaces it. Replaced files are kept as `NAME.bak`, replaced skills as `<skill>.bak`, and `config.yaml` is always kept as `config.yaml.bak` before it is edited.
- **Added to** (`~`): the user profile and an agent's toolsets are only ever added to, never cut.

`config.yaml` is edited in place, the same way `habakkuk setup` edits it, so your comments and settings stay.

Copied files follow the same idea. A second `--files copy` brings over only what is new or changed in the old folder, and shows `=` for a folder with nothing to copy. It never deletes from the copy and never replaces a file that changed there since. The import keeps a small record, `migrated-files.json` in `$HABAKKUK_HOME`, of which folder went where. Later runs read it, with or without `--files`, so text that already names the copies is seen as already there.

## Troubleshooting

**The old agent's folder is still needed**: run `habakkuk migrate --dry-run` and read the Files section. Entries marked `-` under "Not moved" are what the imported text and the copied scripts still name there. If your own folders are listed as not copied, run `habakkuk migrate --files copy`. What remains after that is part of the old agent, and each one needs a decision from you:

- its `.env`: the keys the import uses are already in `$HABAKKUK_HOME/.env`. Point the script at that file, or set the other variables there.
- its program, for example a script that calls the old agent's own command: change the script to use `habakkuk run` or `habakkuk send`, or keep the old agent installed.
- its config, logs or session database named in a persona file or note: edit the text. Habakkuk has its own.
- "(the folder itself)": the text names the old folder as a whole. Read that text and decide what it means now.

When the Files section lists nothing under "Not moved" and every folder shows `=`, nothing that was imported names the old folder any more. Check that the old agent is stopped and that your schedules ran once from the new place before you remove it.

**"not enough room in ..."**: the copy needs more free space than the disk has. Free some space, pass `--files-dir` with a folder on a bigger disk, or use `--files skip` and move the files yourself.

**A file was left out as too large**: raise the limit, for example `--files-max 8G`, or `--files-max 0` for no limit, and run `habakkuk migrate --files copy` again. Only the missing files are copied.

**A reference was not rewritten**: only paths that start with the old folder are found. A script that builds the path from pieces, or reads it from a variable of the old agent, is not changed. A folder name with a space is followed when the text writes it plainly or in quotes (`"~/.hermes/My Notes/todo.md"`), not when the space is escaped (`My\ Notes`); the plan lists that one under "Not moved" and asks you to edit the text. Search your copied scripts for the old folder's name and fix what is left by hand.

**"found both Hermes Agent and OpenClaw"**: pass `--from hermes` or `--from openclaw`, and run it again for the other one if you want both.

**"found no setup to import"**: the home is somewhere else. Pass `--from` and `--source DIR`.

**"config.yaml does not parse as YAML" or "openclaw.json does not parse as JSON5"**: the source config is damaged. Fix it (the message says where), then run the import again.

**"... does not load, so the import cannot edit it safely"**: your Habakkuk `config.yaml` has an error. Run `habakkuk doctor`, fix it, and run the import again.

**An agent shows "no model found"**: its provider has no Habakkuk equivalent, or it used a subscription sign-in. Its files are imported anyway; give it a model with `habakkuk setup model -agent NAME`, then run `habakkuk migrate` again to add its channels.

**A skill shows "the security scan flagged ..."**: the import uses the same scan as `habakkuk skills install`. Read the flagged file; if you trust it, install it with `habakkuk skills install PATH` (add `-force` for high-risk findings).

**A schedule's delivery was dropped**: it sent to a channel that was not imported. Add the channel, then recreate the schedule with `habakkuk schedules add ... -to CHANNEL:CHAT`.

**Variables "the config names but ... held no value for"**: they were set in your shell, not in the old `.env`. Add them to `$HABAKKUK_HOME/.env`.
