---
title: Get started
description: "What Habakkuk is, how it stays up, and where to go first for what you want to do."
order: 1
---

Habakkuk is an AI agent you run on your own computer or server. You talk to it in a chat app (WhatsApp, Telegram, Slack, Discord, Signal, Matrix, email, SMS, Microsoft Teams, Google Chat, iMessage and more), in the terminal, on a web dashboard or in the desktop app. It uses tools, remembers what matters, runs work on a schedule, and can be one of several agents that hand each other work.

It's one program, with nothing else to install. It's also built so that a crash doesn't cost you anything. Every message is saved as a job in a SQLite queue before the agent starts on it, so a restart picks up where it stopped, even after a `kill -9`.

## Pick your goal

| I want to | Start with |
|---|---|
| Try it in a few minutes | [Quickstart](quickstart.md) |
| Install it on macOS, Linux, Windows or Docker | [Installation](installation.md) |
| Talk to it on WhatsApp | [WhatsApp](../channels/whatsapp.md) |
| Talk to it on Telegram, Slack, Discord or another app | [Channels](../channels/index.md) |
| See what you can set up with it | [Use cases](use-cases.md) |
| Bring my setup over from another agent | [Move from another agent](migrate.md) |
| Use a local model, or a cheap one for quick messages | [Providers](../models/providers.md) and [routing](../models/routing.md) |
| Run a team of agents | [Multi-agent teams](../agents/multi-agent.md) |
| Keep it running on a server | [Run it on a server](../deploy/server.md) |
| Understand the config file | [Configuration](configuration.md) |

## Why Habakkuk

- **One program.** A single static binary. There's no runtime to install and nothing to compile (no CGO).
- **A crash doesn't lose work.** Every turn is a job that a worker holds a lease on. If the process dies, the next start reclaims the job. A tool call that was cut off is repaired, so it isn't blindly run again.
- **Storage that stays small.** Live state sits in a small `control.db`, old conversations in `transcripts.db`, and files in a content-addressed store that keeps one copy of each. Old data is cleaned up by default.
- **Chat apps that stay connected.** Every connection runs under a supervisor that watches its heartbeat. A stuck connection is torn down and opened again.
- **One reply per message.** A message that arrives twice is answered once. A long reply that a crash interrupted resumes at the first part you haven't received.
- **Any model.** Claude, GPT, Gemini, DeepSeek, Grok, Mistral, Qwen, Bedrock, OpenRouter, local models and more, with a cheap model for quick messages if you want one.
- **Config that catches typos.** A key Habakkuk doesn't know is reported, so a misspelt setting can't sit there doing nothing.

## How it stays up

- **Durable turns.** A message becomes a `turn` job. Workers claim jobs with a lease and heartbeat while running. Every transcript write checks the lease, so a worker that lost its job can never write.
- **Fast crash recovery.** Leases record host and PID. On start, Habakkuk reclaims jobs whose owner process is dead instead of waiting for the lease to expire.
- **Transcript repair.** A tool call whose result was never recorded gets an "interrupted, check state first" result, so side effects are not blindly repeated.
- **Model resilience.** Errors are classified. Transient ones retry with jittered backoff, rate limits rotate keys with cooldowns, and errors another model can fix fall back down the chain.
- **Cache-stable prompts.** The system prompt is frozen per session, tools are sorted, the time goes in the user turn, and transcripts are append-only.

[Architecture](../reference/architecture.md) explains the design in depth.

## Next steps

<!-- cards -->
- [Quickstart](quickstart.md): install, run `hk setup`, and get a first reply.
- [Use cases](use-cases.md): eight setups with the config and commands for each.
- [Move from another agent](migrate.md): bring an existing agent setup with you.
