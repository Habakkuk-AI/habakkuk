---
title: Agents and identity
description: "Define an agent, give it a personality and standing instructions, switch personas, and see how the first-chat welcome lets you name it."
order: 1
---

An agent is defined in your config, and a few files shape how it talks. Come here when you set up a new agent, or when you want to change one's settings, personality or language.

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

Each agent keeps its files in `$HABAKKUK_HOME/agents/<name>/`:

| File | What it is |
|---|---|
| `SOUL.md` | The agent's personality. `hk init` writes one for the default agent |
| `AGENTS.md` | Standing instructions |
| `IDENTITY.md` | The name, emoji and manner the user chose in chat |
| `USER.md` | The agent's profile of the user. See [Memory](memory.md#the-user-profile) |

Habakkuk reads these files when a session starts. Your edits reach every new session (`/new`), and you don't need to restart. A session that's already open keeps the prompt it started with, so the provider's prompt cache stays valid.

### Personas

Send `/personality NAME` in a chat to add a persona to the end of the session's prompt. The built-in personas are `concise`, `warm`, `formal`, `technical` and `playful`. You can also write your own as `$HABAKKUK_HOME/personalities/NAME.md`, which is read on every turn. `/personality off` drops the persona, and `/new` starts a session without it.

## The first chat

The first one-to-one conversation an agent has with you opens with a welcome. That conversation can be a direct message on any channel, `hk chat` or the TUI. The agent greets you and introduces itself. It suggests three things to try right away, each written as a message you could send. It picks them from the tools it actually has, and puts a morning brief first when it can schedule tasks. Then it asks three questions: what to call you, whether it should keep its name and how it should talk, and where you are, for time zones. If your first message asks for something real, the agent answers that first.

Your answers go to `agents/<name>/IDENTITY.md` (the name, emoji and manner, written through the `set_identity` tool) and to the user profile, `USER.md`. You can change the identity later too, by saying something like "call yourself Jarvis". The prompt states the identity after `SOUL.md`. That way a soul you wrote by hand keeps its place, and the name you chose wins.

> [!NOTE]
> Habakkuk refuses `set_identity` in a turn that has read outside content, because the identity goes into every prompt.

The welcome runs once per agent. Habakkuk marks it as done with an `.onboarding` file. So if two chats open at the same moment, you get one welcome. And if you ignore the questions, the agent doesn't ask again. An agent that already has an identity or a user profile skips the welcome. An agent moved over from another setup is one example. Automated sessions, such as schedules, webhooks and the board, never get it. On Telegram, `/start` reaches the agent for as long as it hasn't met you. After that, it shows the command list.

## Languages

Habakkuk tells the model to answer in the user's language. Habakkuk's own text is handled separately. That text is the command replies, approval prompts and pairing notices. It comes from catalogs for English, French, Spanish, Portuguese, German, Yoruba, Arabic, Chinese (Simplified), Japanese and Hindi. The non-English catalogs are machine-translated. Set `language:` in the config to choose the default. A channel's own `language:` overrides it.

```yaml
language: fr
channels:
  telegram:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN
    language: es        # Habakkuk's own messages on this channel
```
