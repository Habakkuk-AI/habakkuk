---
title: Configuration
description: "How config.yaml is laid out, where keys go, what lives in $HABAKKUK_HOME, and which changes need a restart."
order: 4
---

Habakkuk reads `$HABAKKUK_HOME/config.yaml`. The default home is `~/.habakkuk` (`%APPDATA%\Habakkuk` on Windows). `habakkuk setup` writes a short config holding only what you chose; `habakkuk init` writes a starter file with comments for every section. `habakkuk doctor` checks the config, the keys it names and the channels.

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

The three main sections:

- **`providers`** are the model APIs. An agent's `model` is `<provider>/<model id>`. See [Model providers](../models/providers.md).
- **`agents`** are the assistants, each with its models, permission mode, workspace and toolsets. See [Agents and identity](../agents/agents.md) and [Permissions and approvals](../agents/approvals.md).
- **`channels`** are keyed by an account name you choose; `type` says which platform. See [Channels](../channels/index.md).

The [configuration reference](../reference/configuration.md) lists every section.

## Keys and secrets

Keys are read from the environment or `$HABAKKUK_HOME/.env`, never from the config file. Settings that end in `_env` name the variable that holds a value (`token_env: TELEGRAM_BOT_TOKEN`), so `config.yaml` can be shared or committed without secrets.

```bash
# $HABAKKUK_HOME/.env
ANTHROPIC_API_KEY=sk-ant-...
TELEGRAM_BOT_TOKEN=123456:ABC...
```

`secrets:` can resolve those variables from 1Password, Bitwarden, `pass` or any command instead; see [Secret managers](../deploy/security.md#secret-managers).

## Typos and bad values

A key Habakkuk does not know is reported, not silently accepted: `habakkuk doctor` prints `config: UNKNOWN KEY ...`, and commands such as `habakkuk serve` and `habakkuk chat` warn about it when they start. The key is then ignored, so a setting written for a newer release does not stop an older binary.

Bad values (an unknown provider in a model name, a permission mode that does not exist, a channel without its token setting) stop the config from loading, and every problem is reported at once. `habakkuk serve` keeps running on the last good config when a hand edit breaks the file, and logs the error.

## What lives in `$HABAKKUK_HOME`

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

[Architecture: storage](../reference/architecture.md#storage) describes each file.

## What reloads without a restart

`SOUL.md`, `AGENTS.md`, `USER.md` and the memory and skill listings are read when a session starts, and persona files on every turn. Their edits reach every new session (`/new`) without a restart; an existing session keeps its frozen prompt.

`habakkuk serve` checks `$HABAKKUK_HOME` every 3 seconds:

- Hand-edited memory notes are reindexed at once.
- `hooks:` and spending limits (`budget:`) apply in place.
- Changes to providers, agents, channels, MCP servers, `http`, transcription, timezone, workers, retention, metrics, telemetry, logging, tools and secrets are logged as needing a restart.
- Other sections are read at start, so restart `serve` after changing them.
- An invalid `config.yaml` is reported and ignored.

## Other common settings

```yaml
timezone: America/Toronto   # for schedules and budgets; default the server's
language: fr                # Habakkuk's own chat and CLI text; a channel's language overrides it
workers:
  concurrency: 4
  lease: 45s
retention:
  days: 90                  # transcripts and finished jobs older than this are pruned; 0 disables
```
