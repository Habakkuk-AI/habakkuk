---
title: Overview
description: "Habakkuk is a durable personal AI agent in one Go binary: many chat channels, tools, memory, skills, schedules and a job queue that survives kill -9."
order: 1
---

A durable personal AI agent in one Go binary. Habakkuk talks to you over the CLI, Telegram, Slack, WhatsApp (as a linked device or as a WhatsApp agent), Discord, Signal, Matrix, email, SMS, Microsoft Teams, Google Chat, iMessage, LINE, Mattermost, IRC, ntfy, the WhatsApp Business Cloud API, Messenger, Instagram, Feishu/Lark, DingTalk, WeCom, Zulip, Rocket.Chat, Nextcloud Talk, Twitch chat and a small web chat, uses tools, and survives crashes: every turn is a job with a lease, so a `kill -9` at any moment loses nothing and a restart picks up where it stopped.

## Why Habakkuk

- **One binary.** One static binary, no CGO and no runtime to install.
- **Crash-proof by design.** Every turn is a leased job; a dead process's work is reclaimed on restart and interrupted tool calls are repaired, never blindly repeated.
- **Storage that stays small.** Hot `control.db`, cold `transcripts.db` and content-addressed blobs, with retention on by default.
- **Channels that stay connected.** Every adapter runs under a supervisor with a heartbeat watchdog; a stuck socket is torn down and reconnected, not left hanging.
- **Never answers twice.** Redelivered messages are dropped, replies are delivered once, and a long reply resumes at the first unsent chunk after a crash.
- **Strict config.** Unknown keys are errors, not silent typos.

## How it stays up

- **Durable turns.** A message becomes a `turn` job. Workers claim jobs with a lease and heartbeat while running. Every transcript write checks the lease, so a worker that lost its job can never write.
- **Fast crash recovery.** Leases record host and PID; on start, Habakkuk reclaims jobs whose owner process is dead instead of waiting for expiry.
- **Transcript repair.** A tool call whose result was never recorded gets an "interrupted, check state first" result, so side effects are not blindly repeated.
- **Model resilience.** Errors are classified; transient ones retry with jittered backoff, rate limits rotate keys with cooldowns, and errors another model can fix fall back down the chain.
- **Cache-stable prompts.** The system prompt is frozen per session, tools are sorted, the time goes in the user turn, and transcripts are append-only.

## Where to go next

**Getting started**

- [Installation](getting-started/installation.md): the install script, release archives, `go install` and the container image.
- [Quick start](getting-started/quickstart.md): set up a provider, chat, and the everyday commands.
- [Configuration](getting-started/configuration.md): `config.yaml`, keys in `.env`, and where everything lives.

**Guides**

- [Channels](guides/channels.md), [WhatsApp](guides/whatsapp.md), and [Telegram, Slack, Discord and more](guides/chat-platforms.md)
- [Agents, identity and the first chat](guides/agents.md) and [models and providers](guides/models.md)
- [Memory and skills](guides/memory-and-skills.md), [tools and approvals](guides/tools-and-approvals.md), [MCP servers](guides/mcp.md) and [plugins](guides/plugins.md)
- [Schedules and the task board](guides/schedules-and-board.md) and [multi-agent teams](guides/multi-agent.md)
- [Browser and computer use](guides/browser-and-computer.md), [meetings](guides/meetings.md) and [voice](guides/voice.md)
- [The dashboard](guides/dashboard.md), [the desktop app](guides/desktop-app.md) and [integrations](guides/integrations.md)
- [Deployment and running as a service](guides/deployment.md)

**Reference**

- [CLI](reference/cli.md), [configuration](reference/configuration.md), [HTTP API](reference/api.md) and [architecture](reference/architecture.md)

**Contributing**

- [Codebase map](contributing/codebase.md), [brand](contributing/brand.md), [roadmap](contributing/roadmap.md), and the repository's [contributing guide](../CONTRIBUTING.md)
