---
title: Models
description: "Connect Habakkuk to model providers, look up the built-in presets, and find the pages on routing, subscriptions, spending limits and speech."
order: 4
---

A provider is a model API. Anthropic, OpenAI and Gemini are providers. So is a local Ollama server, or any OpenAI-compatible service. You list your providers under `providers:` in `config.yaml`. Each agent then names its model as `<provider>/<model id>`.

```yaml
providers:
  anthropic: {}               # a provider named after a preset uses it
  local:
    preset: ollama
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
    fallbacks: [local/llama3.3]
```

Run `habakkuk models` to see each provider and its credential status. Where the API can list its models, you see those too. Habakkuk doesn't assume any one vendor.

## In this section

<!-- cards -->
- [Providers](providers.md): set up a provider by type or preset, add custom and local APIs, and configure key rotation and fallbacks.
- [Routing](routing.md): send quick messages to a cheap model and real work to a strong one.
- [Decision model](decision-model.md): let a fast yes/no model make Habakkuk's own choices, such as routing and routine approvals.
- [Subscription logins](subscriptions.md): sign in with a ChatGPT or GitHub Copilot plan instead of using an API key.
- [Usage and limits](usage-and-limits.md): see tokens and cost per model and agent, and set hard spending caps.
- [Speech](speech.md): set up transcription for voice notes and text-to-speech for spoken replies.

## Provider directory

A preset fills in the API type, base URL and key variable for you. To use one, set `preset:`, or give the provider the same name as the preset. The names in brackets work too.

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
| `llamacpp` (`llama.cpp`) | `http://localhost:9931/v1` (older llama-server builds use 8080) | optional `LLAMA_API_KEY` |
| `litellm` | `http://localhost:4000/v1` | optional `LITELLM_API_KEY` |
| `vercel` (`vercel-ai-gateway`) | `https://ai-gateway.vercel.sh/v1` | `AI_GATEWAY_API_KEY` |
| `cloudflare` (`cloudflare-ai-gateway`) | set `base_url: https://gateway.ai.cloudflare.com/v1/ACCOUNT_ID/GATEWAY_ID/compat` | `CLOUDFLARE_AI_GATEWAY_KEY` |

If you set a field yourself, your value wins over the preset. A service that isn't listed here works too, as long as it speaks one of the [provider types](providers.md#provider-types).
