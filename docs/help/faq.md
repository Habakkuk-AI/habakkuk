---
title: FAQ
description: "Common questions about Habakkuk: models, cost, data, privacy, Windows, local models, several users, and what happens on a crash."
order: 1
---

These are short answers to the questions that come up most. Each answer links to the page that goes deeper, where there is one.

## Which models can I use?

You can use any provider that speaks the Anthropic, OpenAI Chat Completions, OpenAI Responses, Gemini or Bedrock API. Habakkuk has presets for Anthropic, OpenAI, Gemini, Bedrock, OpenRouter, Groq, Together, Fireworks, DeepSeek, xAI, Mistral, Cerebras, DeepInfra, NVIDIA, Hugging Face, Perplexity, Moonshot, Z.ai, MiniMax, Qwen, Vercel and Cloudflare gateways, and local servers (Ollama, LM Studio, vLLM, llama.cpp, LiteLLM). Nothing in it assumes Claude. See [Models](../models/index.md).

## What does it cost?

Habakkuk itself is free and open source, under the Apache License 2.0. What you pay for is what the agent uses at your model providers. Run `hk usage` to see tokens and cost by model and agent. [Spending limits](../models/usage-and-limits.md#spending-limits) cap the cost per day, month and turn. You can cut the bill further with a cheap [`fast_model`](../models/routing.md) for quick messages.

## Can I use my ChatGPT, Copilot or Claude subscription?

Yes for ChatGPT and GitHub Copilot plans. Run `hk auth login chatgpt` or `copilot`. No for Claude plans. Anthropic's terms don't allow third-party apps to use Claude plan credentials, so use an API key there. See [Subscription logins](../models/subscriptions.md).

## Can it run fully local?

Yes. Point a provider at Ollama, LM Studio, vLLM or llama.cpp. Then talk to it through the web chat, the terminal, or a channel that needs no outside service, such as Matrix on your own homeserver. Be aware that small local models struggle with long turns that use tools. That's why many people keep a local model as the `fast_model` and use a hosted one for real work.

## Where is my data?

It's all in `$HABAKKUK_HOME`. That's `~/.habakkuk` on Linux and macOS, and `%APPDATA%\Habakkuk` on Windows. Your config, keys, SQLite databases, memory notes and skills are plain files in that folder. They're created so that only you can read them. See [What lives in $HABAKKUK_HOME](../get-started/configuration.md#what-lives-in-habakkuk_home).

## Does it phone home?

No. Habakkuk sends no usage telemetry, and it checks for updates only when you run `hk update`. It does contact your model providers, your channels, the services you name in `config.yaml`, and pages the agent reads. To list every endpoint, run `hk security -network`. See [Security](../deploy/security.md#no-surprise-network-calls).

## Does it run on Windows?

Yes. It runs natively, and there's an installer (`irm https://habakkuk.ai/install.ps1 | iex`). The one exception is computer use. For that, run Habakkuk in WSL. By default, the commands the agent runs go through PowerShell. See [Installation: Windows](../get-started/installation.md#windows).

## Does it run on a Raspberry Pi or a small VPS?

Yes. There are static builds for Linux on arm64 and amd64. The process is light, because the models run at your provider. `hk talk` works on a Pi that has a microphone and a speaker.

## What happens if it crashes mid-task?

Nothing is lost. Every turn is a job with a lease (a short claim on the job that a worker has to keep renewing). When Habakkuk restarts, it reclaims the jobs whose process died. It repairs tool calls whose result was never recorded, and tells the agent to check state before repeating them. It resumes a half-sent reply at the first unsent chunk. No message is answered twice.

## Can other people talk to my agent?

Yes, in one of two ways. You can add their ids to a channel's `allow_from`. Or they can message the bot, which gives them a pairing code, and you approve it with `hk pair CODE`. People you let in with `allow_from: ["*"]` are guests. Guests never get commands, file writes outside the workspace or computer use. See [Access, groups and delivery](../channels/settings.md#who-may-talk-to-the-bot).

## How do I stop the agent from running commands?

Set `permission: read-only` or `ask` on the agent. Keep in mind that the default, `workspace`, already asks before every command. Turns that come from chat apps need approval for commands even in `full` mode. See [Permissions and approvals](../agents/approvals.md).

## Can I run more than one agent?

Yes. You can run as many as you like in one process. Each has its own model, permissions, memory and skills. You can route channels or conversations to them, switch between them with `/agent NAME`, or let them delegate to each other. See [Multi-agent teams](../agents/multi-agent.md).

## Why does a reply sometimes arrive all at once?

On Telegram and Slack, you see the reply while it's being written, in one message that becomes the reply ([how it works](../channels/settings.md#replies-as-theyre-written)). Other chat apps can't edit messages, so there the reply arrives when the turn finishes. While a long turn runs, you see a typing indicator, a progress line that changes as the work moves on, or a "still working" notice. The dashboard and the terminal UI show the reply and each tool call live.

## Can I run two copies of `hk serve`?

Not on the same `$HABAKKUK_HOME`. Most channels let only one program poll them, and a second `serve` makes them fight. Run one `serve` per home. If you need two, give each its own home (`HABAKKUK_HOME=...`). Separate homes are fully independent.

## How do I move to a new machine?

Run `hk backup -env -workspaces -o habakkuk.tar.gz` on the old machine. Then run `hk restore habakkuk.tar.gz` on the new one. See [Backups and updates](../deploy/backups-and-updates.md#moving-to-another-machine).

## I use another agent today. Can I bring my setup?

Yes. [Move from another agent](../get-started/migrate.md) shows you how.
