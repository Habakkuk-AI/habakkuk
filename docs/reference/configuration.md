---
title: Configuration
description: "Every section of config.yaml, the settings for providers, agents and channels, and where each one is explained."
order: 2
---

`$HABAKKUK_HOME/config.yaml` is strict YAML: unknown keys are errors. Settings ending in `_env` name an environment variable (or a `.env` entry, or a [secret manager](../guides/deployment.md#secret-managers) reference) that holds the value; secrets never go in the file. `habakkuk init` writes a commented starter config, and `habakkuk doctor` checks one.

## Top-level sections

| Key | What it configures | Guide |
|---|---|---|
| `version` | The config format version; must be `1` | |
| `providers` | Model APIs, keyed by a name you choose | [Models and providers](../guides/models.md) |
| `agents` | Agents, keyed by name | [Agents](../guides/agents.md) |
| `channels` | Chat accounts, keyed by account name | [Channels](../guides/channels.md) |
| `mcp_servers` | MCP servers whose tools agents can use | [MCP servers](../guides/mcp.md) |
| `mcp_server` | Habakkuk as an MCP server over HTTP (`http`, `allow_send`) | [MCP servers](../guides/mcp.md#habakkuk-as-an-mcp-server) |
| `http` | The HTTP server: `listen`, `token_env`, `public_url`, `webhooks` | [Dashboard and web chat](../guides/dashboard.md) |
| `transcription` | Speech-to-text for voice notes and `habakkuk talk`: `provider`, `model`, `prompt` | [Models and providers](../guides/models.md#speech-to-text) |
| `tts` | Speech for `speak` and voice replies: `provider`, `key_env`, `base_url`, `model`, `voice`, `format`, `instructions`, `max_chars` | [Voice](../guides/voice.md) |
| `realtime` | Live voice mode in the dashboard: `provider`, `model`, `voice`, `key_env`, `base_url`, `transcription`, `instructions`, `ttl` | [Voice](../guides/voice.md#live-mode-in-the-dashboard) |
| `talk` | `habakkuk talk`: `wake`, `follow_up`, `silence`, `threshold`, `input`, `device`, `player`, `barge_in` | [Voice](../guides/voice.md) |
| `images` | `image_generate`: `provider`, `key_env`, `base_url`, `model`, `edit_model`, `size` | [Tools and approvals](../guides/tools-and-approvals.md#media-tools) |
| `video` | `video_generate`: `provider`, `key_env`, `base_url`, `model`, `image_model`, `size`, `seconds`, `timeout` | [Tools and approvals](../guides/tools-and-approvals.md#media-tools) |
| `search` | `web_search`: `provider`, `key_env`, `url` | [Tools and approvals](../guides/tools-and-approvals.md#media-tools) |
| `browser` | The browser toolset: `mode`, `path`, `cdp_url`, `headless`, `args`, `allowed_urls`, `blocked_urls`, `allow_private`, `idle_timeout`, `timeout` | [Browser and computer use](../guides/browser-and-computer.md) |
| `computer` | The computer toolset: `backend`, `display`, `screen`, `max_actions`, `confirm_apps`, `native`, `timeout` | [Browser and computer use](../guides/browser-and-computer.md#computer) |
| `meeting` | Meeting attendance: `name`, `announce`, `max_duration`, `lobby_timeout`, `alone_timeout`, `headless`, `summary_model` | [Meetings](../guides/meetings.md) |
| `memory` | `embeddings` (`provider`, `model`, `dimensions`), `review`, `review_model`, `review_idle`, `nightly`, `stale_days` | [Memory and skills](../guides/memory-and-skills.md) |
| `skills` | `registries`, `default_registry`, `curate`, `archive_after_days` | [Memory and skills](../guides/memory-and-skills.md#skills) |
| `hooks` | A list of `{event, command, timeout, match}` | [Tools and approvals](../guides/tools-and-approvals.md#hooks) |
| `pricing` | Model prices per million tokens: `input`, `output`, `cache_read`, `cache_write` | [Models and providers](../guides/models.md#usage-and-cost) |
| `a2a` | Agent2Agent: `serve`, `token_env`, `name`, `description`, `agents`, `peers` | [Integrations](../guides/integrations.md#agent2agent) |
| `homeassistant` | `url`, `token_env`, `allow_domains`, `confirm`, `triggers` | [Integrations](../guides/integrations.md#home-assistant) |
| `nodes` | Device nodes: `url`, `allow`, `timeout`, `pair_ttl` | [Integrations](../guides/integrations.md#device-nodes) |
| `timezone` | The time zone for schedules, e.g. `America/Toronto` (default the server's) | [Schedules](../guides/schedules-and-board.md) |
| `language` | The default language of Habakkuk's own text | [Agents](../guides/agents.md#languages) |
| `workers` | `concurrency` and job `lease` | |
| `retention` | `days`: transcripts and finished jobs older than this are pruned; `0` disables | [Deployment](../guides/deployment.md#retention) |
| `metrics` | `public`: serve `/metrics` without the API token | [Deployment](../guides/deployment.md#metrics-traces-and-logs) |
| `telemetry` | OTLP traces: `otlp_endpoint`, `headers_env`, `include_content`, `service_name` | [Deployment](../guides/deployment.md#metrics-traces-and-logs) |
| `logging` | `max_mb`, `backups` for `habakkuk.log` rotation | [Deployment](../guides/deployment.md#metrics-traces-and-logs) |
| `tools` | `allow_private_urls` for `fetch_url` and A2A | [Tools and approvals](../guides/tools-and-approvals.md#safety-guards) |
| `secrets` | `sources` and `refs` for secret managers | [Deployment](../guides/deployment.md#secret-managers) |

Enabled plugins add MCP servers, skills, hooks and commands to the loaded configuration without changing the file; see [Plugins](../guides/plugins.md).

## Providers

| Key | Meaning |
|---|---|
| `type` | `anthropic`, `openai` (any OpenAI-compatible API), `responses`, `gemini` or `bedrock` |
| `preset` | A known service that fills in type, base URL and key variable; a provider named after a preset uses it |
| `base_url` | The API endpoint |
| `keys_env` | Variables holding keys, tried in order and rotated on rate limits and auth failures |
| `auth` | `chatgpt` or `copilot` for a subscription login instead of keys |
| `region` | `bedrock`: the AWS region (default `AWS_REGION` or the shared config) |

## Agents

| Key | Default | Meaning |
|---|---|---|
| `model` | | `<provider>/<model id>` |
| `fallbacks` | | Models tried in order after `model` |
| `effort` | | `low`, `medium`, `high`, `xhigh` or `max` |
| `max_tokens` | 64000 | Output tokens per model call |
| `max_iterations` | 200 | Model calls per turn |
| `permission` | `workspace` | `read-only`, `ask`, `workspace` or `full` |
| `approvals` | `human` | `human`, `reviewer` or `reviewer_then_human` |
| `reviewer_model` | the agent's chain | The approval reviewer's model |
| `workspace` | | The agent's working folder |
| `toolsets` | `core`, `memory`, `skills`, `schedule`, `delegate`, `board` | The tools it gets; a list replaces the default |
| `web` | `true` | Use the provider's hosted web search and fetch |
| `compact_at` | 600000 on `anthropic`, else 150000 | Prompt size that triggers compaction; `-1` turns it off |
| `sandbox` | `none` | `none`, `docker` (with `docker: {image, network, cpus, memory, user}`) or `ssh` (with `ssh: {host, port, user, key_env, workdir}`) |
| `checkpoints` | `true` | Snapshot the workspace before turns change it |
| `checkpoint_max_mb` | 200 | Skip a snapshot larger than this |
| `tool_search_over` | 60 | Switch to tool search above this many tools; `-1` turns it off |
| `title_model` | the agent's chain | The model that writes session titles; `off` disables them |

## Channels

| Key | Meaning |
|---|---|
| `type` | The platform: see [Channels](../guides/channels.md#supported-types) |
| `agent` | The default agent for this account's conversations |
| `routes` | Conversation (or Slack team) id to agent |
| `allow_from` | Sender ids allowed to talk; `"*"` allows everyone |
| `groups` | `off`, `mention` (default) or `all` |
| `session_idle` | Fresh session after this much silence (default 24h; web never; `0` never) |
| `token_env` | Telegram and Discord bot token, Matrix access token, WhatsApp agent API key |
| `app_token_env`, `bot_tokens_env` | Slack: the Socket Mode app token, and a bot token per workspace |
| `account`, `homeserver` | Signal: the bot's number; Matrix: its user id and the homeserver URL |
| `api_url` | Override the platform API base URL; Signal: the signal-cli daemon |
| `voice_replies`, `voice_text` | Spoken replies: `off`, `mirror` or `always`, and whether the text is sent too |
| `progress` | `draft` (default) or `off` |
| `reactions` | React to the user's message when a turn starts and ends |
| `language` | The language of Habakkuk's own messages on this channel |
| `email`, `sms`, `msteams`, `googlechat`, `bluebubbles`, `line`, `mattermost`, `irc`, `ntfy`, `whatsappcloud`, `messenger`, `instagram`, `feishu`, `dingtalk`, `wecom`, `zulip`, `rocketchat`, `nextcloudtalk`, `twitch` | The block for that type; see [Telegram, Slack, Discord and more](../guides/chat-platforms.md) |

The source of truth is `internal/config`: each setting is documented beside its field.
