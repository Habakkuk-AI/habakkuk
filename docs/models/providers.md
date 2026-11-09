---
title: Providers
description: "Provider types and presets, custom OpenAI-compatible APIs, local model servers, AWS Bedrock, key rotation, fallbacks, and switching models in a chat."
order: 1
---

## Provider types

A provider's `type` is the API it speaks:

| `type` | API |
|---|---|
| `anthropic` | The Anthropic Messages API |
| `openai` | Any OpenAI-compatible Chat Completions API |
| `responses` | The OpenAI Responses API |
| `gemini` | Google's generateContent API |
| `bedrock` | AWS Bedrock Converse, signed with the standard AWS credential chain |

## Presets

For well-known services, `preset:` fills in the type, base URL and key variable, and a provider named after a preset uses it without saying so. The [provider directory](index.md#provider-directory) lists them all.

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

Any field you set wins over the preset.

## Custom APIs

A service without a preset works if it speaks one of the types above. Give it a type, a base URL and the variable holding its key:

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

Ollama, LM Studio, vLLM, llama.cpp's `llama-server` and LiteLLM have presets pointing at their usual local ports, with the key optional. `habakkuk setup` finds servers running on this computer.

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
> Small local models often struggle with long tool-using turns. A common setup keeps a local model as the [`fast_model`](routing.md) for quick messages and a hosted model for real work.

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

Errors are classified: rate limit, overload, auth, billing, context overflow, bad request, network.

- **Transient errors** retry with jittered backoff.
- **Rate limits and auth failures** rotate to the next key in `keys_env`, with a cooldown per key.
- **Errors another model can fix** fall back down the agent's `fallbacks`, in order.

See [Architecture: models](../reference/architecture.md#models).

## Switching models in a chat

`/model` shows the models the conversation uses. `/model provider/model` switches this conversation only (the agent's own models stay as fallbacks), and `/model default` switches back. The choice is stored per session and overrides [routing](routing.md).

## Checking providers

```bash
habakkuk models      # each provider, its credential status, and the models it serves
habakkuk doctor      # "provider X: 2/2 keys present" or what is missing
```
