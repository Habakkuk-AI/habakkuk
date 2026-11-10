---
title: Configuration
description: "How config.yaml is laid out, where keys go, what lives in $HABAKKUK_HOME, and which changes need a restart."
order: 4
---

Read this when you want to change settings by hand. It covers how Habakkuk's config file is laid out, where your keys go, and which changes need a restart.

Habakkuk reads its settings from `$HABAKKUK_HOME/config.yaml`. The default home is `~/.habakkuk`, or `%APPDATA%\Habakkuk` on Windows. There are two ways to get a config. `habakkuk setup` writes a short one that holds only what you chose. `habakkuk init` writes a starter file with comments for every section. To check the config, the keys it names and the channels, run `habakkuk doctor`.

## A first config

```yaml
version: 1
providers:
  anthropic:
    type: anthropic
    keys_env: [ANTHROPIC_API_KEY, ANTHROPIC_API_KEY_2]   # rotated on rate limits
  deepseek:
    type: openai                                         # any OpenAI-compatible API
    base_url: https://api.deepseek.com
    keys_env: [DEEPSEEK_API_KEY]
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5     # real work
    fast_model: deepseek/deepseek-flash  # quick messages and chores
    fallbacks: [deepseek/deepseek-v4-pro]
    effort: high
    permission: workspace     # read-only | ask | workspace | full
    approvals: reviewer_then_human   # human (default) | reviewer | reviewer_then_human
    reviewer_model: anthropic/claude-sonnet-5-5   # default: the agent's own models
    workspace: ~/habakkuk-workspace
channels:
  telegram:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN
    allow_from: ["123456789"]   # or leave empty and approve people with habakkuk pair
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN              # Socket Mode, one connection for every workspace
    bot_tokens_env: [SLACK_BOT_TOKEN, SLACK_BOT_TOKEN_2]
    routes: {T0123456: ops}                     # a workspace or conversation can go to another agent
  whatsapp:
    type: whatsapp
    groups: mention                             # off | mention | all
retention:
  days: 90
```

A config has three main sections:

- **`providers`** lists the model APIs. An agent's `model` is written as `<provider>/<model id>`. See [Model providers](../models/providers.md).
- **`agents`** defines the assistants. Each one has its own models, permission mode, workspace and toolsets. See [Agents and identity](../agents/agents.md) and [Permissions and approvals](../agents/approvals.md).
- **`channels`** holds one entry per account, under a name you choose. Its `type` says which platform it is. See [Channels](../channels/index.md).

For every section, see the [configuration reference](../reference/configuration.md).

## Keys and secrets

Habakkuk reads keys from the environment or from `$HABAKKUK_HOME/.env`. It doesn't read them from the config file. A setting that ends in `_env` gives the name of the variable that holds the value, as in `token_env: TELEGRAM_BOT_TOKEN`. That keeps secrets out of `config.yaml`, so you can share it or commit it.

```bash
# $HABAKKUK_HOME/.env
ANTHROPIC_API_KEY=sk-ant-...
TELEGRAM_BOT_TOKEN=123456:ABC...
```

If you'd rather keep those variables in 1Password, Bitwarden, `pass` or behind any command, `secrets:` can resolve them from there. See [Secret managers](../deploy/security.md#secret-managers).

## Typos and bad values

If your config has a key Habakkuk doesn't know, it tells you. `habakkuk doctor` prints `config: UNKNOWN KEY ...`, and commands such as `habakkuk serve` and `habakkuk chat` warn about it when they start. Habakkuk then ignores the key, so a setting written for a newer release doesn't stop an older binary.

Bad values are different: they stop the config from loading. Examples are an unknown provider in a model name, a permission mode that doesn't exist, or a channel without its token setting. Habakkuk reports every problem at once. If a hand edit breaks the file while `habakkuk serve` is running, it logs the error and keeps running on the last good config.

## What lives in `$HABAKKUK_HOME`

Here's what you'll find in the home folder:

| Path | What it holds |
|---|---|
| `config.yaml`, `.env` | Configuration and secrets |
| `agents/<name>/SOUL.md`, `AGENTS.md`, `IDENTITY.md`, `USER.md` | An agent's personality, standing instructions, chosen name and its profile of you |
| `agents/<name>/memory/` | Memory notes, one Markdown file each |
| `agents/<name>/skills/`, `skills/` | The agent's own skills and shared skills |
| `plugins/` | Installed plugins |
| `personalities/` | Your own `/personality` files |
| `control.db` | Jobs, conversation bindings, channel health, allowlist, delivery progress, schedules, the board |
| `transcripts.db`, `state.db`, `web.db`, `usage.db`, `nodes.db`, `memory.db`, `history.db` | Sessions, per-session state, web chat log, token usage, device nodes, the memory search index, memory change history |
| `blobs/` | Large tool outputs and media |
| `channels/` | Per-channel state (WhatsApp credentials, sync positions) |
| `auth.json` | Subscription and MCP OAuth logins |
| `habakkuk.log` | The log, rotated, with secrets redacted |

For a description of each file, see [Architecture: storage](../reference/architecture.md#storage).

## What reloads without a restart

Habakkuk reads `SOUL.md`, `AGENTS.md`, `USER.md` and the memory and skill listings when a session starts. It reads persona files on every turn. So your edits to them reach every new session (start one with `/new`) without a restart. A session that's already open keeps its frozen prompt, the one it started with.

While it runs, `habakkuk serve` checks `$HABAKKUK_HOME` every 3 seconds. What it does depends on what changed:

- Memory notes you edited by hand are reindexed at once.
- Changes to `hooks:` and to spending limits (`budget:`) apply in place.
- Changes to providers, agents, channels, MCP servers, `http`, transcription, timezone, workers, retention, metrics, telemetry, logging, tools and secrets need a restart, and the log says so.
- Other sections are read at start, so restart `serve` after you change them.
- If `config.yaml` is invalid, Habakkuk reports it and ignores the file.

## Other common settings

A few more settings you're likely to reach for:

```yaml
timezone: America/Toronto   # for schedules and budgets; default the server's
language: fr                # Habakkuk's own chat and CLI text; a channel's language overrides it
workers:
  concurrency: 4
  lease: 45s
retention:
  days: 90                  # transcripts and finished jobs older than this are pruned; 0 disables
```
