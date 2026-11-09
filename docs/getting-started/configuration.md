---
title: Configuration
description: "How config.yaml is laid out, where keys go, what lives in $HABAKKUK_HOME, and which changes need a restart."
order: 3
---

Habakkuk reads `$HABAKKUK_HOME/config.yaml` (default `~/.habakkuk/config.yaml`). `habakkuk init` writes a starter file with comments for every section; `habakkuk doctor` checks it, the keys it names and the channels.

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
    fast_model: deepseek/deepseek-flash  # quick messages and chores (Routing in the models guide)
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

- **`providers`** are the model APIs; an agent's `model` is `<provider>/<model id>`. See [Models and providers](../guides/models.md).
- **`agents`** are the assistants, each with its models, permission mode, workspace and toolsets. See [Agents](../guides/agents.md) and [Tools and approvals](../guides/tools-and-approvals.md).
- **`channels`** are keyed by an account name you choose; `type` says which platform. See [Channels](../guides/channels.md).

The [configuration reference](../reference/configuration.md) lists every section.

## Keys and secrets

Keys are read from the environment or `$HABAKKUK_HOME/.env`, never from the config file. Settings that end in `_env` name the variable that holds a value (`token_env: TELEGRAM_BOT_TOKEN`), so `config.yaml` can be shared or committed without secrets. `secrets:` can resolve those variables from 1Password, Bitwarden, `pass` or any command instead; see [Deployment](../guides/deployment.md#secret-managers).

## Strict checking

Unknown keys are errors, not silent typos. A config that does not load is reported with every problem at once. `habakkuk serve` keeps running on the last good config when a hand edit breaks the file, and reports the error.

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
| `transcripts.db`, `state.db`, `web.db`, `usage.db`, `nodes.db`, `memory.db` | Sessions, per-session state, web chat log, token usage, device nodes, the memory search index |
| `blobs/` | Large tool outputs and media |
| `channels/` | Per-channel state (WhatsApp credentials, sync positions) |
| `auth.json` | Subscription and MCP OAuth logins |
| `habakkuk.log` | The log, rotated, with secrets redacted |

[Architecture: storage](../reference/architecture.md#storage) describes each file.

## What reloads without a restart

SOUL.md, AGENTS.md, USER.md and the memory and skill listings are read when a session starts, and persona files on every turn, so their edits reach every new session (`/new`) without a restart; an existing session keeps its frozen prompt. `habakkuk serve` polls `$HABAKKUK_HOME` every 3 seconds: hand-edited memory notes are reindexed at once, `hooks:` changes apply in place, and changes to any other section of `config.yaml` (providers, agents, channels, MCP servers, http, transcription, timezone, workers, retention) are logged as needing a restart. An invalid `config.yaml` is reported and ignored.

## Other common settings

```yaml
timezone: America/Toronto   # for schedules; default the server's
language: fr                # Habakkuk's own chat and CLI text; a channel's language overrides it
workers:
  concurrency: 4
  lease: 45s
retention:
  days: 90                  # transcripts and finished jobs older than this are pruned; 0 disables
```
