---
title: Providers
description: "Set up providers: types and presets, custom OpenAI-compatible APIs, local model servers, AWS Bedrock, key rotation, fallbacks and switching models in a chat."
order: 1
---

A provider is a model API that Habakkuk calls. Come here when you want to add a service, run a local model server, rotate keys or set fallbacks.

## Provider types

Every provider has a `type`, which says which API it speaks:

| `type` | API |
|---|---|
| `anthropic` | The Anthropic Messages API |
| `openai` | Any OpenAI-compatible Chat Completions API |
| `responses` | The OpenAI Responses API |
| `gemini` | Google's generateContent API |
| `bedrock` | AWS Bedrock Converse, signed with the standard AWS credential chain |

## Presets

For well-known services, you don't have to fill in the details yourself. Set `preset:` and it fills in the type, base URL and key variable. If you name a provider after a preset, it uses that preset without you saying so. The [provider directory](index.md#provider-directory) lists every preset.

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

If you set a field yourself, your value wins over the preset.

## Custom APIs

A service that has no preset still works, as long as it speaks one of the types above. Give it a type, a base URL and the variable that holds its key:

```yaml
providers:
  myproxy:
    type: openai
    base_url: https://llm.example.com/v1
    keys_env: [MYPROXY_API_KEY]
agents:
  habakkuk:
    model: myproxy/some-model-id
```

## Local model servers

Ollama, LM Studio, vLLM, llama.cpp's `llama-server` and LiteLLM each have a preset. The preset points at the server's usual local port, and the key is optional. When you run `hk setup`, it finds servers running on this computer.

```yaml tab="Ollama"
providers:
  ollama: {}                  # http://localhost:11434/v1
agents:
  habakkuk:
    model: ollama/llama3.3
```

```yaml tab="LM Studio"
providers:
  lmstudio: {}                # http://localhost:1234/v1
agents:
  habakkuk:
    model: lmstudio/qwen3-30b
```

```yaml tab="Another machine"
providers:
  gpu:
    preset: vllm
    base_url: http://gpu-box.lan:8000/v1
agents:
  habakkuk:
    model: gpu/meta-llama/Llama-3.3-70B-Instruct
```

> [!TIP]
> Small local models often struggle with long turns that use tools. A common setup uses a local model as the [`fast_model`](routing.md) for quick messages, and a hosted model for real work.

## Keys, retries and fallbacks

```yaml
providers:
  anthropic:
    keys_env: [ANTHROPIC_API_KEY, ANTHROPIC_API_KEY_2]
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
    fallbacks: [deepseek/deepseek-v4-pro, ollama/llama3.3]
```

Habakkuk sorts every error into a class: rate limit, overload, auth, billing, context overflow, bad request or network. What it does next depends on the class:

- **Transient errors** are retried with jittered backoff (a randomized wait between tries).
- **Rate limits and auth failures** make Habakkuk rotate to the next key in `keys_env`. Each key has its own cooldown.
- **Errors another model can fix** send the call down the agent's `fallbacks`, in order.

When every model in the chain turns the call down because it's out of credit or its key or login was rejected, retrying won't help, so the turn stops at once. The reply says which providers and why, for example: "no model could answer: deepseek is out of credit and anthropic is out of credit." A key set aside after one of these errors stays set aside for 30 minutes, and later calls keep naming the real reason.

When that happens to a schedule or other background work, its chat hears about it once, not on every run. You'll hear again only if the same providers are still refused a day later, or after one of them has worked and a new outage starts. A message you send yourself always gets the answer. A restart forgets what you were told, so you may hear once more after one. To keep an agent working through an empty account, give it a fallback on another provider.

For more, see [Architecture: models](../reference/architecture.md#models).

## Switching models in a chat

Send `/model` to see the models the conversation uses. Send `/model provider/model` to switch models for this conversation only. The agent's own models stay as fallbacks. Send `/model default` to switch back. Your choice is stored per session, and it overrides [routing](routing.md).

## Checking providers

Use these two commands to check your providers:

```bash
hk models            # each provider, its credential status, and the models it serves
hk doctor            # "provider X: 2/2 keys present" or what is missing
```
