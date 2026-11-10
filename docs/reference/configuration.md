---
title: Configuration
description: "Look up every section of config.yaml, the settings for providers, agents and channels, and the guide that explains each one."
order: 2
---

Every section of the config file is listed here, with the settings for providers, agents and channels. Use it to look up a key. Where a section has a guide, the table links to it.

The config file is `$HABAKKUK_HOME/config.yaml`. It's strict YAML, so an unknown key is an error. A setting that ends in `_env` doesn't hold the value itself. It names an environment variable that holds it. A `.env` entry or a [secret manager](../deploy/security.md#secret-managers) reference works too. That way, secrets never go in the file. `hk init` writes a commented starter config, and `hk doctor` checks one.

## Top-level sections

| Key | What it configures | Guide |
|---|---|---|
| `version` | The config format version; must be `1` | |
| `providers` | Model APIs, keyed by a name you choose | [Models and providers](../models/providers.md) |
| `agents` | Agents, keyed by name | [Agents](../agents/agents.md) |
| `channels` | Chat accounts, keyed by account name | [Channels](../channels/index.md) |
| `mcp_servers` | MCP servers whose tools agents can use | [MCP servers](../tools/mcp.md) |
| `mcp_server` | Habakkuk as an MCP server over HTTP (`http`, `allow_send`) | [MCP servers](../tools/mcp.md#habakkuk-as-an-mcp-server) |
| `http` | The HTTP server: `listen`, `token_env`, `public_url`, `webhooks`, `push_contact` (the `mailto:` address or `https` URL sent with [Web Push](../interfaces/dashboard.md#notifications); default `public_url` when it's `https`) | [Dashboard and web chat](../interfaces/dashboard.md) |
| `transcription` | Speech-to-text for voice notes and `hk talk`: `provider`, `model`, `prompt` | [Models and providers](../models/speech.md#speech-to-text) |
| `tts` | Speech for `speak` and voice replies: `provider`, `key_env`, `base_url`, `model`, `voice`, `format`, `instructions`, `max_chars` | [Voice](../tools/voice.md) |
| `realtime` | Live voice mode in the dashboard: `provider`, `model`, `voice`, `key_env`, `base_url`, `transcription`, `instructions`, `ttl` | [Voice](../tools/voice.md#live-mode-in-the-dashboard) |
| `talk` | `hk talk`: `wake`, `follow_up`, `silence`, `threshold`, `input`, `device`, `player`, `barge_in` | [Voice](../tools/voice.md) |
| `images` | `image_generate`: `provider`, `key_env`, `base_url`, `model`, `edit_model`, `size` | [Tools and approvals](../tools/media.md) |
| `video` | `video_generate`: `provider`, `key_env`, `base_url`, `model`, `image_model`, `size`, `seconds`, `timeout` | [Tools and approvals](../tools/media.md) |
| `search` | `web_search`: `provider`, `key_env`, `url` | [Tools and approvals](../tools/media.md) |
| `browser` | The browser toolset: `mode`, `path`, `cdp_url`, `headless`, `args`, `allowed_urls`, `blocked_urls`, `allow_private`, `idle_timeout`, `timeout` | [Browser and computer use](../tools/browser.md) |
| `computer` | The computer toolset: `backend`, `display`, `screen`, `max_actions`, `confirm_apps`, `native`, `timeout` | [Browser and computer use](../tools/computer-use.md) |
| `meeting` | Meeting attendance: `name`, `announce`, `max_duration`, `lobby_timeout`, `alone_timeout`, `headless`, `summary_model` | [Meetings](../tools/meetings.md) |
| `memory` | `embeddings` (`provider`, `model`, `dimensions`), `review`, `review_model`, `review_idle`, `nightly`, `stale_days`, `history_days` | [Memory and skills](../agents/memory.md) |
| `skills` | `registries`, `default_registry`, `curate`, `archive_after_days` | [Memory and skills](../agents/skills.md) |
| `retro` | Retrospectives on a timer: `every`, `at`, `agents`, `model`, `apply`, `deliver` | [Retros](../agents/retros.md) |
| `hooks` | A list of `{event, command, timeout, match}` | [Tools and approvals](../agents/approvals.md#hooks) |
| `pricing` | Model prices per million tokens: `input`, `output`, `cache_read`, `cache_write` | [Models and providers](../models/usage-and-limits.md#usage-and-cost) |
| `a2a` | Agent2Agent: `serve`, `token_env`, `name`, `description`, `agents`, `peers` | [Integrations](../tools/integrations.md#agent2agent) |
| `homeassistant` | `url`, `token_env`, `allow_domains`, `confirm`, `triggers` | [Integrations](../tools/integrations.md#home-assistant) |
| `nodes` | Device nodes: `url`, `allow`, `timeout`, `pair_ttl` | [Integrations](../tools/integrations.md#device-nodes) |
| `timezone` | The time zone for schedules, e.g. `America/Toronto` (default the server's) | [Schedules](../agents/schedules.md), [Usage and limits](../models/usage-and-limits.md#spending-limits) |
| `budget` | Spending caps for the whole install: `daily`, `monthly`, `per_turn`, `unknown_price` | [Usage and limits](../models/usage-and-limits.md#spending-limits) |
| `judge` | A decision model for routing, routine approvals and memory reviews: `model`, `key_env`, `timeout_ms`, `routing`, `approvals`, `memory`, `approve_above`, `skip_memory_below` | [Decision model](../models/decision-model.md) |
| `language` | The default language of Habakkuk's own text | [Agents](../agents/agents.md#languages) |
| `workers` | `concurrency` and job `lease` | |
| `retention` | `days`: transcripts and finished jobs older than this are pruned; `0` disables | [Deployment](../deploy/backups-and-updates.md#retention) |
| `metrics` | `public`: serve `/metrics` without the API token | [Deployment](../deploy/observability.md#metrics-and-traces) |
| `telemetry` | OTLP traces: `otlp_endpoint`, `headers_env`, `include_content`, `service_name` | [Deployment](../deploy/observability.md#metrics-and-traces) |
| `logging` | `max_mb`, `backups` for `habakkuk.log` rotation | [Deployment](../deploy/observability.md#metrics-and-traces) |
| `tools` | `allow_private_urls` for `fetch_url` and A2A | [Tools and approvals](../agents/approvals.md#safety-guards) |
| `secrets` | `sources` and `refs` for secret managers | [Deployment](../deploy/security.md#secret-managers) |

Plugins you've enabled add MCP servers, skills, hooks and commands to the loaded configuration. They don't change the file. See [Plugins](../tools/plugins.md).

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
| `fast_model` | | A cheaper model for quick messages and chores ([routing](../models/routing.md)) |
| `routing` | `auto` with a `fast_model` | `auto`, `fast` or `smart` |
| `escalate_after` | 4 | Tool steps a quick turn takes on `fast_model` before moving to `model` |
| `effort` | | `low`, `medium`, `high`, `xhigh` or `max` |
| `max_tokens` | 64000 | Output tokens per model call |
| `max_iterations` | 200 | Model calls per turn |
| `loop_guard` | `repeat_warn: 3`, `repeat_stop: 5`, `idle_warn: 8`, `idle_stop: 12` | Warn, then stop, a turn that repeats the same failing call or rounds that learn nothing new; `-1` turns a check off ([agents](../agents/agents.md#when-an-agent-goes-round-in-circles)) |
| `permission` | `workspace` | `read-only`, `ask`, `workspace` or `full` |
| `approvals` | `human` | `human`, `reviewer` or `reviewer_then_human` |
| `reviewer_model` | the agent's chain | The approval reviewer's model |
| `workspace` | | The agent's working folder |
| `project_files` | `on` | Read the project's `AGENTS.md`, `CLAUDE.md` and `.cursorrules` (up to the git root) into new sessions: `on` or `off`. A file is followed only when its exact content was trusted with `hk projects trust`; an untrusted one makes every command in the session need approval ([agents](../agents/agents.md#project-instruction-files)) |
| `toolsets` | `core`, `memory`, `skills`, `schedule`, `delegate`, `board` | The tools it gets; a list replaces the default |
| `web` | `true` | Use the provider's hosted web search and fetch |
| `compact_at` | 600000 on `anthropic`, else 150000 | Prompt size that triggers compaction; `-1` turns it off |
| `sandbox` | `none` | `none`, `docker` (with `docker: {image, network, cpus, memory, user}`) or `ssh` (with `ssh: {host, port, user, key_env, workdir}`) |
| `checkpoints` | `true` | Snapshot the workspace before turns change it |
| `checkpoint_max_mb` | 200 | Skip a snapshot larger than this |
| `tool_search_over` | 60 | Switch to tool search above this many tools; `-1` turns it off |
| `title_model` | `fast_model`, else the agent's chain | The model that writes session titles; `off` disables them |
| `home`, `report_home` | | The agent's home chat (`CHANNEL:CONVERSATION[:THREAD]`) and what is reported there: `results` (default with a home), `all` or `off` ([multi-agent](../agents/multi-agent.md#teams-and-home-chats)) |
| `budget` | | This agent's spending caps: `daily`, `monthly`, `per_turn` ([limits](../models/usage-and-limits.md#spending-limits)) |
| `trust`, `channel_allow`, `allow_tainted_exec` | | Lift the extra checks on chat-driven and tainted turns ([security](../deploy/security.md)) |
| `trusted_write_paths` | none | Folders outside the workspace where the file tools may save notes without approval; never commands, scripts or hidden files ([approvals](../agents/approvals.md#trusted-folders-for-notes)) |

## Channels

| Key | Meaning |
|---|---|
| `type` | The platform: see [Channels](../channels/index.md#all-channels) |
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
| `progress` | What a chat shows while the agent works: `plain` (default, one short line), `detailed` (a line per tool step; `draft` is its old name) or `off`. Changes apply without a restart ([settings](../channels/settings.md#what-a-chat-shows-while-the-agent-works)) |
| `approvals_detail` | What an approval request shows: `summary` (default, plain words) or `full` (also the exact call). Changes apply without a restart ([settings](../channels/settings.md#approval-requests-in-plain-words)) |
| `reactions` | React to the user's message when a turn starts and ends |
| `language` | The language of Habakkuk's own messages on this channel |
| `email`, `sms`, `msteams`, `googlechat`, `bluebubbles`, `line`, `mattermost`, `irc`, `ntfy`, `whatsappcloud`, `messenger`, `instagram`, `feishu`, `dingtalk`, `wecom`, `zulip`, `rocketchat`, `nextcloudtalk`, `twitch` | The block for that type; see the [channel's page](../channels/index.md#all-channels) |

The source of truth is `internal/config`. Each setting is documented there, beside its field.
