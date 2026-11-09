---
title: Agents, identity and the first chat
description: "Define agents, give them a personality and standing instructions, and how the first-chat welcome lets the user name the agent."
order: 4
---

An agent is a configured assistant: its models, permission mode, workspace, toolsets and the files that shape how it talks. One Habakkuk can run several; see [Multi-agent teams](multi-agent.md).

## Defining an agent

```yaml
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
    fallbacks: [deepseek/deepseek-chat]
    effort: high                    # low | medium | high | xhigh | max
    permission: workspace           # read-only | ask | workspace (default) | full
    approvals: human                # human (default) | reviewer | reviewer_then_human
    workspace: ~/habakkuk-workspace
    toolsets: [core, memory, skills, schedule, delegate, board]   # the default when not set
```

| Setting | Meaning |
|---|---|
| `model`, `fallbacks` | `<provider>/<model id>`, then models tried in order when one fails in a way another can fix. See [Models and providers](models.md) |
| `effort` | The reasoning effort sent with each request; `/think` overrides it per session |
| `max_tokens`, `max_iterations` | Output tokens per model call (default 64000) and model calls per turn (default 200) |
| `permission`, `approvals`, `reviewer_model` | What the agent may do without asking, and who decides when it must ask. See [Tools and approvals](tools-and-approvals.md) |
| `workspace` | The folder its file tools and commands work in |
| `toolsets` | The tools it gets. Without the setting, an agent gets `core`, `memory`, `skills`, `schedule`, `delegate` and `board`; a config that lists `toolsets` gets exactly that list |
| `web` | Use the provider's hosted web search and fetch where the model supports it (default `true`) |
| `compact_at` | Summarize older messages once a prompt reaches this many tokens (default 600000 on `anthropic` providers, 150000 otherwise; `-1` turns it off) |
| `title_model` | The model that writes session titles (default the agent's chain; `off` disables titles) |
| `sandbox`, `checkpoints`, `tool_search_over` | Where commands run, workspace snapshots, and when to switch to tool search. See [Tools and approvals](tools-and-approvals.md) |

## Personality and instructions

An agent's files live in `$HABAKKUK_HOME/agents/<name>/`:

| File | What it is |
|---|---|
| `SOUL.md` | The agent's personality. `habakkuk init` writes one for the default agent |
| `AGENTS.md` | Standing instructions |
| `IDENTITY.md` | The name, emoji and manner the user chose in chat |
| `USER.md` | The agent's profile of the user. See [Memory and skills](memory-and-skills.md) |

They are read when a session starts, so edits reach every new session (`/new`) without a restart; an existing session keeps its frozen prompt, which keeps the provider's prompt cache valid.

**Personas.** `/personality NAME` in a chat appends a persona to the session's prompt: the built-in `concise`, `warm`, `formal`, `technical` and `playful`, or your own `$HABAKKUK_HOME/personalities/NAME.md`, read on every turn. `/new` starts a session without it.

## The first chat

The first one-to-one conversation an agent has with its user (a direct message on any channel, `habakkuk chat` or the TUI) opens with a welcome: the agent greets the user, introduces itself, lists a handful of things it can do with the tools it actually has, and asks three questions: what to call the user, whether to keep its name and how to talk, and where they are for time zones. A first message that asks for something real is answered first.

The answers go to `agents/<name>/IDENTITY.md` (name, emoji, manner, through the `set_identity` tool, which the user can also use later: "call yourself Jarvis") and to the user profile, `USER.md`. The prompt states the identity after SOUL.md, so a hand-written soul keeps its place and the chosen name wins. `set_identity` is refused in a turn that read outside content, since the identity reaches every prompt.

The welcome runs once per agent: Habakkuk marks it with an `.onboarding` file, so two chats opening at once welcome once and a user who ignores the questions is not asked again. An agent that already has an identity or a user profile, such as one moved over from another setup, skips it. Schedules, webhooks, the board and other automated sessions never take it. A Telegram `/start` reaches the agent while it has not met its user, and shows the command list afterwards.

## Sessions

A conversation maps to a session. A chat channel starts a fresh session after `session_idle` of silence (default 24 hours), or when you send `/new`.

- **Compaction.** Long sessions are compacted without rewriting anything: older messages are summarized into a record and the model is sent the summary plus the recent turns; the full transcript stays for `habakkuk show`. `/compact` or `habakkuk sessions compact ID` does it now.
- **Titles.** A session's title starts as its first message; after the first turn a short title is written in the background. `/title [text]` shows or sets it.
- **Fork and rewind.** `/fork` copies the session into a new one to try another direction; `/rewind [N]` does the same without the last N user turns. The original stays as it was.
- **Export and search.** `/export` sends the transcript as Markdown. `habakkuk sessions export ID -format md|json|html`, `habakkuk sessions search QUERY`, and `habakkuk sessions archive|unarchive|rm` manage sessions from the terminal. The `session_search` tool lets an agent search its own past sessions.
- **Standing goals.** `/goal [-turns N] <text>` keeps the agent working on a goal turn after turn (default 10 turns, at most 100) until it replies that the goal is done or blocked.

[Architecture: compaction](../reference/architecture.md#compaction) and [session tools](../reference/architecture.md#session-tools) have the details.

## Languages

The model is told to answer in the user's language. Habakkuk's own text (command replies, approval prompts, pairing notices) comes from catalogs for English, French, Spanish, Portuguese, German, Yoruba, Arabic, Chinese (Simplified), Japanese and Hindi; the non-English ones are machine-translated. Config `language:` sets the default and a channel's `language:` overrides it.
