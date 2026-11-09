---
title: Agents and identity
description: "Define agents, give them a personality and standing instructions, use personas, and how the first-chat welcome lets the user name the agent."
order: 1
---

## Defining an agent

```yaml
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
    fallbacks: [deepseek/deepseek-v4-pro]
    effort: high                    # low | medium | high | xhigh | max
    permission: workspace           # read-only | ask | workspace (default) | full
    approvals: human                # human (default) | reviewer | reviewer_then_human
    workspace: ~/habakkuk-workspace
    toolsets: [core, memory, skills, schedule, delegate, board]   # the default when not set
```

| Setting | Meaning |
|---|---|
| `model`, `fallbacks` | `<provider>/<model id>`, then models tried in order when one fails in a way another can fix. See [Model providers](../models/providers.md) |
| `fast_model`, `routing`, `escalate_after` | A cheaper model for quick messages and chores, and how turns are split between it and `model`. See [Routing](../models/routing.md) |
| `effort` | The reasoning effort sent with each request; `/think` overrides it per session |
| `max_tokens`, `max_iterations` | Output tokens per model call (default 64000) and model calls per turn (default 200) |
| `permission`, `approvals`, `reviewer_model` | What the agent may do without asking, and who decides when it must ask. See [Permissions and approvals](approvals.md) |
| `trust`, `channel_allow`, `allow_tainted_exec` | Lift the extra checks on chat-driven and tainted turns. See [Security](../deploy/security.md) |
| `workspace` | The folder its file tools and commands work in (default `$HABAKKUK_HOME/agents/<name>/workspace`) |
| `toolsets` | The tools it gets. Without the setting: `core`, `memory`, `skills`, `schedule`, `delegate` and `board`. A list replaces the default. See [Tools](../tools/index.md) |
| `web` | Use the provider's hosted web search and fetch where the model supports it (default `true`) |
| `compact_at` | Summarize older messages once a prompt reaches this many tokens (default 600000 on `anthropic` providers, 150000 otherwise; `-1` turns it off) |
| `title_model` | The model that writes session titles (default `fast_model`, else the agent's chain; `off` disables titles) |
| `sandbox`, `checkpoints`, `tool_search_over` | Where commands run, workspace snapshots, and when to switch to tool search. See [Files and shell](../tools/files-and-shell.md) |
| `home`, `report_home` | The agent's own chat, where it reports work that started elsewhere. See [Multi-agent teams](multi-agent.md#teams-and-home-chats) |
| `budget` | Spending caps for this agent. See [Usage and limits](../models/usage-and-limits.md#spending-limits) |

## Personality and instructions

An agent's files live in `$HABAKKUK_HOME/agents/<name>/`:

| File | What it is |
|---|---|
| `SOUL.md` | The agent's personality. `habakkuk init` writes one for the default agent |
| `AGENTS.md` | Standing instructions |
| `IDENTITY.md` | The name, emoji and manner the user chose in chat |
| `USER.md` | The agent's profile of the user. See [Memory](memory.md#the-user-profile) |

They are read when a session starts, so edits reach every new session (`/new`) without a restart. An existing session keeps its frozen prompt, which keeps the provider's prompt cache valid.

### Personas

`/personality NAME` in a chat appends a persona to the session's prompt: the built-in `concise`, `warm`, `formal`, `technical` and `playful`, or your own `$HABAKKUK_HOME/personalities/NAME.md`, read on every turn. `/personality off` drops it, and `/new` starts a session without it.

## The first chat

The first one-to-one conversation an agent has with its user (a direct message on any channel, `habakkuk chat` or the TUI) opens with a welcome. The agent greets the user, introduces itself, lists a handful of things it can do with the tools it actually has, and asks three questions: what to call the user, whether to keep its name and how to talk, and where they are for time zones. A first message that asks for something real is answered first.

The answers go to `agents/<name>/IDENTITY.md` (name, emoji and manner, through the `set_identity` tool) and to the user profile, `USER.md`. The user can change the identity later too: "call yourself Jarvis". The prompt states the identity after `SOUL.md`, so a hand-written soul keeps its place and the chosen name wins.

> [!NOTE]
> `set_identity` is refused in a turn that read outside content, since the identity reaches every prompt.

The welcome runs once per agent. Habakkuk marks it with an `.onboarding` file, so two chats opening at once welcome once, and a user who ignores the questions is not asked again. An agent that already has an identity or a user profile, such as one moved over from another setup, skips it. Schedules, webhooks, the board and other automated sessions never take it. A Telegram `/start` reaches the agent while it has not met its user, and shows the command list afterwards.

## Languages

The model is told to answer in the user's language. Habakkuk's own text (command replies, approval prompts, pairing notices) comes from catalogs for English, French, Spanish, Portuguese, German, Yoruba, Arabic, Chinese (Simplified), Japanese and Hindi; the non-English ones are machine-translated. Config `language:` sets the default, and a channel's `language:` overrides it.

```yaml
language: fr
channels:
  telegram:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN
    language: es        # Habakkuk's own messages on this channel
```
