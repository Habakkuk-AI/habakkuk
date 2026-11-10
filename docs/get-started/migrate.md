---
title: Move from another agent
description: "Bring your agents, memory, skills, schedules, MCP servers, channels and keys over from Hermes Agent or OpenClaw with habakkuk migrate."
order: 7
---

If you already run [Hermes Agent](https://github.com/NousResearch/hermes-agent) or [OpenClaw](https://github.com/openclaw/openclaw), `habakkuk migrate` reads its home folder and sets up the same things in Habakkuk. It shows you a plan first and writes nothing until you say yes. Running it again is safe: what is already there is left alone.

```bash
habakkuk migrate --dry-run     # see what would move, change nothing
habakkuk migrate               # show the plan, ask, then import
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

Source folders are found the way their own tools find them: `~/.hermes` or `$HERMES_HOME`, and `~/.openclaw`, `$OPENCLAW_STATE_DIR` or `~/.openclaw-<profile>` for `$OPENCLAW_PROFILE`. Pass `--source DIR` for anywhere else.

## What does not move

| What | Why | Instead |
|---|---|---|
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
                 [--dry-run] [--secrets copy|skip] [--force] [-y]
```

| Flag | Meaning |
|---|---|
| `--from` | `hermes` or `openclaw`. Without it, the one found on this computer is used; if both are there you must choose. |
| `--source DIR` | The source home folder, when it is not in the default place. |
| `--agent NAME` | What the main agent is called in Habakkuk (default `habakkuk`). Other agents keep their names. |
| `--dry-run` | Print the plan and stop. Nothing is written, not even an empty folder. |
| `--secrets copy` | Copy keys and tokens into `$HABAKKUK_HOME/.env` (readable only by you). |
| `--secrets skip` | Copy no keys; the report lists the variables to set yourself. |
| `--force` | Replace agents, persona files, skills, notes, schedules and `.env` values that already exist and differ. |
| `-y` | Do not ask. Keys are copied only with `--secrets copy`. |

Without `--secrets`, an interactive run asks once whether to copy the keys.

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

Then import, copying the keys:

```console
$ habakkuk migrate --secrets copy -y
...
Imported 2 agents, 3 persona files, 1 profile, 3 memory notes, 2 skills, 2 schedules, 2 MCP servers, 2 providers, 2 channels.
Copied ANTHROPIC_API_KEY, GITHUB_PERSONAL_ACCESS_TOKEN, NOTION_TOKEN, OPENROUTER_API_KEY, TELEGRAM_BOT_TOKEN into /home/ada/.habakkuk/.env.

Schedules are imported paused. Check them with habakkuk schedules list, then resume each one:
  habakkuk schedules resume sch_6f9580dd273695f7    # habakkuk/Inbox sweep
  habakkuk schedules resume sch_4066676d3e5680b4    # habakkuk/Morning brief

Next:
  habakkuk doctor      check the config, keys and channels
  habakkuk serve       start the agents and connect the channels
```

> [!WARNING]
> Stop the old agent before `habakkuk serve` starts the same channels. A Telegram, Discord or Slack bot can only be polled by one program at a time, and two agents answering one chat confuses everyone.

Schedules are imported paused so nothing runs twice while both agents are installed. A script schedule gets a ten-minute time limit (or its interval, when an `@every` interval is shorter) instead of the usual two minutes, because the old agent let scripts run longer. Resume each one once the old agent is stopped. A script schedule still runs the script from the old folder; move it if you plan to delete that folder.

After the import, read each agent's `SOUL.md` and `AGENTS.md`: instructions written for the old agent may name its tools or commands, which Habakkuk does not have.

## Running it again

The import compares everything with what is already in `$HABAKKUK_HOME`:

- **Already there** (`=`): the same file, note, skill, schedule, server or channel exists. Nothing happens.
- **Conflict** (`!`): something different exists under the same name, for example a `SOUL.md` you wrote. It is left alone and listed; `--force` replaces it. Replaced files are kept as `NAME.bak`, replaced skills as `<skill>.bak`, and `config.yaml` is always kept as `config.yaml.bak` before it is edited.
- **Added to** (`~`): the user profile and an agent's toolsets are only ever added to, never cut.

`config.yaml` is edited in place, the same way `habakkuk setup` edits it, so your comments and settings stay.

## Troubleshooting

**"found both Hermes Agent and OpenClaw"**: pass `--from hermes` or `--from openclaw`, and run it again for the other one if you want both.

**"found no setup to import"**: the home is somewhere else. Pass `--from` and `--source DIR`.

**"config.yaml does not parse as YAML" or "openclaw.json does not parse as JSON5"**: the source config is damaged. Fix it (the message says where), then run the import again.

**"... does not load, so the import cannot edit it safely"**: your Habakkuk `config.yaml` has an error. Run `habakkuk doctor`, fix it, and run the import again.

**An agent shows "no model found"**: its provider has no Habakkuk equivalent, or it used a subscription sign-in. Its files are imported anyway; give it a model with `habakkuk setup model -agent NAME`, then run `habakkuk migrate` again to add its channels.

**A skill shows "the security scan flagged ..."**: the import uses the same scan as `habakkuk skills install`. Read the flagged file; if you trust it, install it with `habakkuk skills install PATH` (add `-force` for high-risk findings).

**A schedule's delivery was dropped**: it sent to a channel that was not imported. Add the channel, then recreate the schedule with `habakkuk schedules add ... -to CHANNEL:CHAT`.

**Variables "the config names but ... held no value for"**: they were set in your shell, not in the old `.env`. Add them to `$HABAKKUK_HOME/.env`.
