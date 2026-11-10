---
title: Routing
description: "Give an agent a fast_model so Habakkuk sends quick messages and chores to a cheap model and keeps the strong one for real work."
order: 2
---

Routing lets one agent use two models. Give the agent a `fast_model`, and Habakkuk picks a model for every message. Any provider works on either side. For the quick side, you might use DeepSeek, Gemini Flash, a local Ollama model or Claude Haiku. For the real work, you might use Claude, GPT, Gemini Pro or DeepSeek Pro.

```yaml
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5     # real work
    fast_model: deepseek/deepseek-flash  # quick messages and chores
    routing: auto                        # auto (default) | fast | smart
    escalate_after: 4                    # tool steps before a quick turn moves to model
```

> [!TIP]
> If you've configured a [decision model](decision-model.md), it decides between quick and real work, in well under a second. It takes the place of the word rules below.

## How `auto` decides

- **Quick messages go to `fast_model`.** These are greetings, questions, reminders and small lookups.
- **Real work goes to `model`.** That covers a message that asks to build, code, debug, fix, research, analyse, compare, plan, design, review or write something long (a report, a proposal, an article). It covers a message with code, or one over about 600 characters. It covers attachments, including earlier ones in the conversation, since a fast model may not see images. Browser and computer tasks count as real work too, and so do automations.
- **Follow-ups stay with the work.** If you send a short reply such as "yes, go ahead" within 15 minutes of real work, it stays on `model`.
- **A quick turn that turns into work moves up.** Once a turn has taken `escalate_after` tool steps on `fast_model`, the rest of it runs on `model`.
- **Failures fall back.** After the fast model, the chain carries on with `model` and `fallbacks`. An outage, or an error the main model can fix, goes there.
- **Chores use the fast model.** Chores are session titles, compaction summaries and memory review. To send them somewhere else, set `title_model` or `memory.review_model`. The approval reviewer is different: it keeps the agent's main chain unless you set `reviewer_model`.

## Other modes

| `routing` | What happens |
|---|---|
| `auto` (default with a `fast_model`) | As above |
| `fast` | Every turn goes to `fast_model` first, with `model` only as a fallback and no escalation |
| `smart` | Every turn stays on `model`; `fast_model` is used only for chores |

In a chat, send `/model provider/model` to override routing for that conversation. Send `/model` to see the routing in use. To see the split by model, run `hk usage`.

## What setup picks

When you pick a provider's recommended model in `hk setup`, setup turns routing on for you. It pairs that model with the same provider's cheaper one, on the same key:

| Provider | `model` | `fast_model` |
|---|---|---|
| Anthropic | `claude-opus-5-5` | `claude-haiku-5-5` |
| OpenAI | `gpt-5.6-sol` | `gpt-5.6-luna` |
| Gemini | `gemini-4-argon` | `gemini-3.8-flash` |
| DeepSeek | `deepseek-v4-pro` | `deepseek-flash` |

You can mix providers in the same way. For example, `model: anthropic/claude-opus-5-5` with `fast_model: deepseek/deepseek-flash` works once both keys are in `.env`.
