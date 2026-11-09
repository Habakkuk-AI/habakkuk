---
title: Get started
description: "What Habakkuk is, how it stays up, and where to go first for what you want to do."
order: 1
---

Habakkuk is a personal AI agent in one Go binary. It talks to you over the terminal, a web dashboard, a desktop app and chat apps (WhatsApp, Telegram, Slack, Discord, Signal, Matrix, email, SMS, Microsoft Teams, Google Chat, iMessage and more). It uses tools, remembers what matters, runs work on a schedule, and can run several agents that hand each other work.

Every turn is a job with a lease in a SQLite queue. A `kill -9` at any moment loses nothing: a restart picks up where it stopped.

## Pick your goal

| I want to | Start with |
|---|---|
| Try it in a few minutes | [Quickstart](quickstart.md) |
| Install it on macOS, Linux, Windows or Docker | [Installation](installation.md) |
| Talk to it on WhatsApp | [WhatsApp](../channels/whatsapp.md) |
| Talk to it on Telegram, Slack, Discord or another app | [Channels](../channels/index.md) |
| See what people set up with it | [Use cases](use-cases.md) |
| Bring my setup over from another agent | [Move from another agent](migrate.md) |
| Use a local model, or a cheap one for quick messages | [Providers](../models/providers.md) and [routing](../models/routing.md) |
| Run a team of agents | [Multi-agent teams](../agents/multi-agent.md) |
| Keep it running on a server | [Run it on a server](../deploy/server.md) |
| Understand the config file | [Configuration](configuration.md) |

## Why Habakkuk

- **One binary.** One static binary, no CGO and no runtime to install.
- **Crash-proof by design.** Every turn is a leased job. A dead process's work is reclaimed on restart, and interrupted tool calls are repaired, never blindly repeated.
- **Storage that stays small.** A hot `control.db`, a cold `transcripts.db` and content-addressed blobs, with retention on by default.
- **Channels that stay connected.** Every adapter runs under a supervisor with a heartbeat watchdog. A stuck socket is torn down and reconnected, not left hanging.
- **Never answers twice.** Redelivered messages are dropped, replies are delivered once, and a long reply resumes at the first unsent chunk after a crash.
- **Any model.** Claude, GPT, Gemini, DeepSeek, Grok, Mistral, Qwen, Bedrock, OpenRouter, local models and more, with a cheap model for quick messages if you want one.
- **Config that catches typos.** Keys Habakkuk does not know are reported, not silently accepted.

## How it stays up

- **Durable turns.** A message becomes a `turn` job. Workers claim jobs with a lease and heartbeat while running. Every transcript write checks the lease, so a worker that lost its job can never write.
- **Fast crash recovery.** Leases record host and PID. On start, Habakkuk reclaims jobs whose owner process is dead instead of waiting for the lease to expire.
- **Transcript repair.** A tool call whose result was never recorded gets an "interrupted, check state first" result, so side effects are not blindly repeated.
- **Model resilience.** Errors are classified. Transient ones retry with jittered backoff, rate limits rotate keys with cooldowns, and errors another model can fix fall back down the chain.
- **Cache-stable prompts.** The system prompt is frozen per session, tools are sorted, the time goes in the user turn, and transcripts are append-only.

[Architecture](../reference/architecture.md) explains the design in depth.

## Next steps

<!-- cards -->
- [Quickstart](quickstart.md): install, run `habakkuk setup`, and get a first reply.
- [Use cases](use-cases.md): eight setups with the config and commands for each.
- [Move from another agent](migrate.md): bring an existing agent setup with you.
