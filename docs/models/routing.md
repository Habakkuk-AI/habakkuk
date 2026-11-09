---
title: Routing
description: "Give an agent a fast_model and Habakkuk picks a model for every message: a cheap one for quick messages and chores, a strong one for real work."
order: 2
---

Give an agent a `fast_model` and Habakkuk picks a model for every message. Any provider works on either side: DeepSeek, Gemini Flash, a local Ollama model or Claude Haiku for the quick side; Claude, GPT, Gemini Pro or DeepSeek Pro for the real work.

```yaml
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5     # real work
    fast_model: deepseek/deepseek-flash  # quick messages and chores
    routing: auto                        # auto (default) | fast | smart
    escalate_after: 4                    # tool steps before a quick turn moves to model
```

## How `auto` decides

- **Quick messages go to `fast_model`**: greetings, questions, reminders, small lookups.
- **Real work goes to `model`**: a message that asks to build, code, debug, fix, research, analyse, compare, plan, design, review or write something long (a report, a proposal, an article); a message with code or over about 600 characters; attachments (including earlier ones in the conversation, since a fast model may not see images); browser and computer tasks; and automations.
- **Follow-ups stay with the work.** A short reply such as "yes, go ahead" within 15 minutes of real work stays on `model`.
- **A quick turn that turns into work moves up.** After `escalate_after` tool steps on `fast_model`, the rest of the turn runs on `model`.
- **Failures fall back.** The fast model's chain continues with `model` and `fallbacks`, so an outage or an error the main model can fix goes there.
- **Chores use the fast model**: session titles, compaction summaries and memory review, unless `title_model` or `memory.review_model` says otherwise. The approval reviewer keeps the agent's main chain unless `reviewer_model` is set.

## Other modes

| `routing` | What happens |
|---|---|
| `auto` (default with a `fast_model`) | As above |
| `fast` | Every turn goes to `fast_model` first, with `model` only as a fallback and no escalation |
| `smart` | Every turn stays on `model`; `fast_model` is used only for chores |

`/model provider/model` in a chat overrides routing for that conversation, and `/model` shows the routing in use. `habakkuk usage` shows the split by model.

## What setup picks

`habakkuk setup` turns routing on when you pick a provider's recommended model, pairing it with that provider's cheaper one on the same key:

| Provider | `model` | `fast_model` |
|---|---|---|
| Anthropic | `claude-opus-5-5` | `claude-haiku-5-5` |
| OpenAI | `gpt-5.6-sol` | `gpt-5.6-luna` |
| Gemini | `gemini-4-argon` | `gemini-3.8-flash` |
| DeepSeek | `deepseek-v4-pro` | `deepseek-flash` |

Mixing providers works the same way: `model: anthropic/claude-opus-5-5` with `fast_model: deepseek/deepseek-flash` needs both keys in `.env`.
