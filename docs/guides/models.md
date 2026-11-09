---
title: Models and providers
description: Provider types and presets, subscription logins, switching models per session, and token usage and cost.
order: 5
---

## Providers

A provider's `type` is the API it speaks: `anthropic`, `openai` (any OpenAI-compatible Chat Completions API), `responses` (the OpenAI Responses API), `gemini` (Google's generateContent API) or `bedrock` (AWS Bedrock Converse, signed with the standard AWS credential chain). For well-known services, `preset:` fills in the type, base URL and key variable, and a provider named after a preset uses it without saying so:

```yaml
providers:
  groq: {}                    # same as preset: groq
  fast:
    preset: cerebras
  local:
    preset: ollama            # no key needed
  gemini:
    keys_env: [GEMINI_API_KEY, GEMINI_API_KEY_2]
  aws:
    type: bedrock
    region: us-east-1         # AWS credentials from the usual chain; keys_env may name Bedrock API keys instead
```

| Preset | Base URL | Key variable |
|---|---|---|
| `anthropic` | Anthropic API | `ANTHROPIC_API_KEY` |
| `openai` | `https://api.openai.com/v1` | `OPENAI_API_KEY` |
| `gemini` (`google`) | Gemini API (type `gemini`) | `GEMINI_API_KEY` |
| `bedrock` | AWS (type `bedrock`) | AWS credential chain |
| `openrouter` | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` |
| `groq` | `https://api.groq.com/openai/v1` | `GROQ_API_KEY` |
| `together` | `https://api.together.ai/v1` | `TOGETHER_API_KEY` |
| `fireworks` | `https://api.fireworks.ai/inference/v1` | `FIREWORKS_API_KEY` |
| `deepseek` | `https://api.deepseek.com` | `DEEPSEEK_API_KEY` |
| `xai` (`grok`) | `https://api.x.ai/v1` | `XAI_API_KEY` |
| `mistral` | `https://api.mistral.ai/v1` | `MISTRAL_API_KEY` |
| `cerebras` | `https://api.cerebras.ai/v1` | `CEREBRAS_API_KEY` |
| `deepinfra` | `https://api.deepinfra.com/v1/openai` | `DEEPINFRA_API_KEY` |
| `nvidia` (`nim`) | `https://integrate.api.nvidia.com/v1` | `NVIDIA_API_KEY` |
| `huggingface` (`hf`) | `https://router.huggingface.co/v1` | `HF_TOKEN` |
| `perplexity` | `https://api.perplexity.ai` | `PERPLEXITY_API_KEY` |
| `moonshot` (`kimi`) | `https://api.moonshot.ai/v1` | `MOONSHOT_API_KEY` |
| `zai` (`glm`) | `https://api.z.ai/api/paas/v4` | `ZAI_API_KEY` |
| `minimax` | `https://api.minimax.io/v1` | `MINIMAX_API_KEY` |
| `qwen` (`dashscope`) | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` (keys are region-bound; set `base_url` for your region) | `DASHSCOPE_API_KEY` |
| `ollama` | `http://localhost:11434/v1` | optional `OLLAMA_API_KEY` |
| `lmstudio` | `http://localhost:1234/v1` | optional `LM_API_TOKEN` |
| `vllm` | `http://localhost:8000/v1` | optional `VLLM_API_KEY` |
| `llamacpp` | `http://localhost:9931/v1` (older llama-server builds use 8080) | optional `LLAMA_API_KEY` |
| `litellm` | `http://localhost:4000/v1` | optional `LITELLM_API_KEY` |
| `vercel` | `https://ai-gateway.vercel.sh/v1` | `AI_GATEWAY_API_KEY` |
| `cloudflare` | set `base_url: https://gateway.ai.cloudflare.com/v1/ACCOUNT_ID/GATEWAY_ID/compat` | `CLOUDFLARE_AI_GATEWAY_KEY` |

Any field you set wins over the preset. `habakkuk models` lists each provider with its credential status and, where the API can list them, its models.

## Retries, key rotation and fallback

Errors are classified (rate limit, overload, auth, billing, context overflow, bad request, network). Transient ones retry with jittered backoff; rate limits and auth failures rotate to the next key in `keys_env`, with a cooldown per key; and errors another model can fix fall back down the agent's `fallbacks`. See [Architecture: models](../reference/architecture.md#models).

## Subscription logins

A provider can use an account sign-in instead of keys:

```bash
habakkuk auth login chatgpt     # ChatGPT plan: device code, then the Responses API the plan uses
habakkuk auth login copilot     # GitHub Copilot plan: GitHub device code, then Copilot's OpenAI-compatible API
habakkuk auth list
habakkuk auth logout chatgpt
```

```yaml
providers:
  chatgpt:
    auth: chatgpt             # type responses is implied
  copilot:
    auth: copilot             # type openai is implied
```

Tokens are kept in `$HABAKKUK_HOME/auth.json` (owner-only, replaced atomically) and refreshed shortly before they expire, under a file lock so two Habakkuk processes never refresh at once. There is no Claude subscription login: Anthropic's terms do not allow third-party apps to use Claude plan credentials, so use an API key.

## Switching models

In a chat, `/model` shows the models the conversation uses and `/model provider/model` switches this conversation only (the agent's own models stay as fallbacks); `/model default` switches back. The choice is stored per session.

## Routing: a cheap model for quick messages, a strong one for real work

Give an agent a `fast_model` and Habakkuk picks a model for every message. Any provider works on either side: DeepSeek, Gemini Flash, a local Ollama model or Claude Haiku for the quick side; Claude, GPT, Gemini Pro or DeepSeek Pro for the real work.

```yaml
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5     # real work
    fast_model: deepseek/deepseek-flash  # quick messages and chores
    routing: auto                        # auto (default) | fast | smart
    escalate_after: 4                    # tool steps before a quick turn moves to model
```

With `routing: auto`:

- **Quick messages go to `fast_model`**: greetings, questions, reminders, small lookups.
- **Real work goes to `model`**: a message that asks to build, code, debug, fix, research, analyse, compare, plan, design, review or write something long (a report, a proposal, an article), a message with code or over about 600 characters, attachments (including earlier ones in the conversation, since a fast model may not see images), browser and computer tasks, and automations.
- **Follow-ups stay with the work.** A short reply such as "yes, go ahead" within 15 minutes of real work stays on `model`.
- **A quick turn that turns into work moves up.** After `escalate_after` tool steps on `fast_model`, the rest of the turn runs on `model`.
- **Failures fall back.** The fast model's chain continues with `model` and `fallbacks`, so an outage or an error the main model can fix goes there.
- **Chores use the fast model**: session titles, compaction summaries and memory review, unless `title_model` or `memory.review_model` says otherwise. The approval reviewer keeps the agent's main chain unless `reviewer_model` is set.

`routing: fast` sends every turn to `fast_model` first, with `model` only as a fallback and no escalation. `routing: smart` keeps every turn on `model` and uses `fast_model` only for chores. `/model provider/model` in a chat overrides routing for that conversation; `/model` shows the routing in use. `habakkuk usage` shows the split by model.

`habakkuk setup` turns routing on when you pick a provider's recommended model, pairing it with that provider's cheaper one on the same key:

| Provider | `model` | `fast_model` |
|---|---|---|
| Anthropic | `claude-opus-5-5` | `claude-haiku-5-5` |
| OpenAI | `gpt-5.6-sol` | `gpt-5.6-luna` |
| Gemini | `gemini-4-argon` | `gemini-3.8-flash` |
| DeepSeek | `deepseek-v4-pro` | `deepseek-flash` |

Mixing providers works the same way: `model: anthropic/claude-opus-5-5` with `fast_model: deepseek/deepseek-flash` needs both keys in `.env`.

## Usage and cost

Every model call's tokens (input, output, cache read and write) are recorded with its provider, model, agent and session in `usage.db`. `habakkuk usage [-days 30] [-agent A] [-session S]` prints totals and cost by model and by agent; `/usage` in a chat shows the current conversation. Costs come from a built-in table of list prices (standard tier, US dollars per million tokens), with long-prompt tiers where providers have them; calls through a subscription login count as $0. Add or correct prices in config:

```yaml
pricing:
  llama-4-scout: {input: 0.11, output: 0.34}
  openrouter/anthropic/claude-sonnet-5-5: {input: 2, output: 10, cache_read: 0.1, cache_write: 2.5}
```

## Spending limits

Hard caps on what model calls cost, so a schedule or a runaway tool loop cannot run up a bill overnight. Set them for the whole install and per agent, in US dollars (numbers or strings: `5`, `"5.00"`):

```yaml
timezone: America/Toronto   # days start at 00:00 here, months on the 1st

budget:                     # the whole install
  daily: 5
  monthly: 100
  per_turn: 1.50            # one turn: its tool loop, compaction, title and sub-agents
  unknown_price: allow      # allow | block (see below)

agents:
  habakkuk:
    budget: {daily: 2, per_turn: 0.50}   # this agent's own calls
```

- **Every model call is checked first**: turns, sub-agents, compaction, automatic titles, the approval reviewer, memory review and meeting notes all go through the same router, which asks the limits before sending. Once a limit is reached the call is not sent. A call already under way is not cut off, so spend can pass a limit by what the calls running at that moment cost, and then stops.
- **Spend is kept in memory**, loaded from `usage.db` at start and updated as each call finishes, so the check costs nothing measurable. Prices are the ones `habakkuk usage` uses.
- **A capped turn ends cleanly** with a reply saying which limit is used up, when it resets and where to raise it, for example: *Paused: the daily budget of $5.00 for agent habakkuk is used up; it resets at 00:00 America/Toronto. Raise it with agents.habakkuk.budget.daily in config.yaml.* The job finishes; it is not retried. A per-turn limit stops only that turn; the next message starts a fresh allowance.
- **Background work tells you once.** A scheduled task whose agent is out of budget is skipped without starting, and its chat gets one notice per limit and period; later skips are only logged. A board card is blocked with the reason. Memory review is skipped and tried again later.
- **Who pays.** A call counts toward the agent that made it and toward the install. A sub-agent's calls count toward the sub-agent's daily and monthly limits, and toward the per-turn limit of the turn that delegated to it.
- **Warning at 80%.** When a daily or monthly limit passes 80%, the chat whose call crossed it gets one warning per period (the log, when there is no chat).
- **Unknown prices.** A model missing from the price table counts as $0, with one warning per model in the log; add it under `pricing:`. With `unknown_price: block`, calls to such models are refused while any limit applies (the next model in the chain is tried).
- **Changing limits** in `config.yaml` applies within seconds, without a restart.

See spend against the limits with `habakkuk usage` (today and this month for the install and each agent), `/budget` in a chat (this conversation's agent and the install), or `GET /api/budget` ([API](docs/API.md#get-apibudgetagent)).

## Speech-to-text

Voice notes from chats and `habakkuk talk` are transcribed through a provider of type `openai` with an `/audio/transcriptions` endpoint:

```yaml
transcription:
  provider: openai          # a provider with type: openai
  model: gpt-4o-mini-transcribe   # required
  prompt: "Names and words to spell right: Habakkuk, Toronto"   # optional
```
