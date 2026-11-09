---
title: FAQ
description: "Common questions about Habakkuk: models, cost, data, privacy, Windows, local models, several users, and what happens on a crash."
order: 1
---

## Which models can I use?

Any provider that speaks the Anthropic, OpenAI Chat Completions, OpenAI Responses, Gemini or Bedrock API. There are presets for Anthropic, OpenAI, Gemini, Bedrock, OpenRouter, Groq, Together, Fireworks, DeepSeek, xAI, Mistral, Cerebras, DeepInfra, NVIDIA, Hugging Face, Perplexity, Moonshot, Z.ai, MiniMax, Qwen, Vercel and Cloudflare gateways, and local servers (Ollama, LM Studio, vLLM, llama.cpp, LiteLLM). Nothing assumes Claude. See [Models](../models/index.md).

## What does it cost?

Habakkuk is free and open source (Apache License 2.0). You pay your model providers for what the agent uses. `habakkuk usage` shows tokens and cost by model and agent, and [spending limits](../models/usage-and-limits.md#spending-limits) cap it per day, month and turn. A cheap [`fast_model`](../models/routing.md) for quick messages cuts the bill further.

## Can I use my ChatGPT, Copilot or Claude subscription?

ChatGPT and GitHub Copilot plans, yes: `habakkuk auth login chatgpt` or `copilot`. Claude plans, no: Anthropic's terms do not allow third-party apps to use Claude plan credentials, so use an API key. See [Subscription logins](../models/subscriptions.md).

## Can it run fully local?

Yes. Point a provider at Ollama, LM Studio, vLLM or llama.cpp, and use the web chat, the terminal or a channel that needs no outside service, such as Matrix on your own homeserver. Small local models struggle with long tool-using turns, so many people keep a local model as the `fast_model` and a hosted one for real work.

## Where is my data?

Everything lives in `$HABAKKUK_HOME`: `~/.habakkuk` on Linux and macOS, `%APPDATA%\Habakkuk` on Windows. Config, keys, SQLite databases, memory notes and skills are plain files there, created readable only by you. See [What lives in $HABAKKUK_HOME](../get-started/configuration.md#what-lives-in-habakkuk_home).

## Does it phone home?

No. Habakkuk sends no usage telemetry and checks for updates only when you run `habakkuk update`. It contacts your model providers, your channels, the services you name in `config.yaml`, and pages the agent reads. `habakkuk security -network` lists every endpoint. See [Security](../deploy/security.md#no-surprise-network-calls).

## Does it run on Windows?

Yes, natively, with an installer (`irm https://habakkuk.ai/install.ps1 | iex`). Computer use is the exception: run Habakkuk in WSL for that. Commands the agent runs use PowerShell by default. See [Installation: Windows](../get-started/installation.md#windows).

## Does it run on a Raspberry Pi or a small VPS?

Yes. There are static builds for Linux on arm64 and amd64, and the process is light: the models run at your provider. `habakkuk talk` works on a Pi with a microphone and speaker.

## What happens if it crashes mid-task?

Nothing is lost. Every turn is a job with a lease. On restart Habakkuk reclaims jobs whose process died, repairs tool calls whose result was never recorded (the agent is told to check state before repeating them), and resumes a half-sent reply at the first unsent chunk. A message is never answered twice.

## Can other people talk to my agent?

Yes. Add their ids to a channel's `allow_from`, or let them message the bot and approve the pairing code they get with `habakkuk pair CODE`. People let in by `allow_from: ["*"]` are guests: they never get commands, file writes outside the workspace or computer use. See [Access, groups and delivery](../channels/settings.md#who-may-talk-to-the-bot).

## How do I stop the agent from running commands?

Set `permission: read-only` or `ask` on the agent. The default, `workspace`, already asks before every command, and turns from chat apps need approval for commands even in `full` mode. See [Permissions and approvals](../agents/approvals.md).

## Can I run more than one agent?

Yes, as many as you like in one process, each with its own model, permissions, memory and skills. Route channels or conversations to them, switch with `/agent NAME`, or let them delegate to each other. See [Multi-agent teams](../agents/multi-agent.md).

## Why do replies arrive all at once?

Replies are delivered when the turn finishes; text is not streamed into chat apps. While a long turn runs you see a typing indicator, a progress message edited with each tool step (Telegram, Slack), or a "still working" notice. The dashboard and terminal UI show tool calls live.

## Can I run two copies of `habakkuk serve`?

Not on the same `$HABAKKUK_HOME`. Most channels allow only one program to poll them, and a second `serve` makes them fight. Use one `serve` per home; separate homes (`HABAKKUK_HOME=...`) are fully independent.

## How do I move to a new machine?

`habakkuk backup -env -workspaces -o habakkuk.tar.gz` on the old one, `habakkuk restore habakkuk.tar.gz` on the new one. See [Backups and updates](../deploy/backups-and-updates.md#moving-to-another-machine).

## I use another agent today. Can I bring my setup?

See [Move from another agent](../get-started/migrate.md).
