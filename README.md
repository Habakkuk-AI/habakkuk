<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/logo-dark.svg">
    <img src="assets/brand/logo.svg" alt="Habakkuk" height="80">
  </picture>
</p>

<p align="center"><strong>A durable personal AI agent in one Go binary.</strong></p>

<p align="center">
  <a href="https://github.com/habakkuk-ai/habakkuk/actions/workflows/ci.yml"><img src="https://github.com/habakkuk-ai/habakkuk/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/habakkuk-ai/habakkuk" alt="Go version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License: Apache-2.0"></a>
</p>

Habakkuk is an open-source AI agent you run yourself. It talks to you over the terminal, a web dashboard, a desktop app and chat apps such as Telegram, Slack, WhatsApp, Discord, Signal, Matrix, email and SMS, and it uses tools, remembers what matters, and runs work on a schedule. Every turn is a job with a lease in a SQLite queue, so a `kill -9` at any moment loses nothing and a restart picks up where it stopped. It ships as one static binary with no CGO and no runtime to install.

## Install

On Linux or macOS (amd64 or arm64):

```bash
curl -fsSL https://habakkuk.ai/install.sh | sh
```

The script checks the release against its `SHA256SUMS` and installs `habakkuk` in `~/.local/bin`. The same script is at `https://raw.githubusercontent.com/habakkuk-ai/habakkuk/main/scripts/install.sh`. You can also use `go install github.com/habakkuk-ai/habakkuk/cmd/habakkuk@latest` or the container image `ghcr.io/habakkuk-ai/habakkuk`; see [Installation](docs/getting-started/installation.md).

## Quick start

```bash
habakkuk setup        # one-minute wizard: model, WhatsApp/Telegram/Slack/..., names
habakkuk chat         # talk to it in the terminal
habakkuk serve        # run channels, schedules, the board and the dashboard
habakkuk doctor       # check config, keys and channels
habakkuk status       # what is running, queue, today's cost
```

## Features

- **Crash-proof work.** Leased jobs, transcript repair, exactly-once replies and resumable deliveries.
- **Many channels.** Telegram, Slack, WhatsApp (linked device, WhatsApp agents, Business Cloud API), Discord, Signal, Matrix, email, SMS, Teams, Google Chat, iMessage, LINE, Mattermost, IRC, ntfy, Messenger, Instagram, Feishu, DingTalk, WeCom, Zulip, Rocket.Chat, Nextcloud Talk, Twitch and a web chat.
- **Any model, and the right one for each message.** Claude, GPT, Gemini, DeepSeek, Grok, Mistral, Qwen, Kimi, GLM, Bedrock, OpenRouter and 20 more presets, or a local model through Ollama, LM Studio, vLLM or llama.cpp. Quick messages go to a cheap model and real work to a strong one, automatically. Key rotation, fallbacks, subscription logins, spending caps and cost tracking.
- **Tools with guardrails.** Permission modes, an approval reviewer, sandboxes, workspace checkpoints, a browser, computer use and meeting notes.
- **Memory and skills.** Markdown notes with semantic search, a user profile, `SKILL.md` skills and plugins.
- **Teams of agents.** Several agents, delegation, schedules and a shared task board.
- **Open protocols.** MCP client and server, Agent2Agent, the Agent Client Protocol and an OpenAI-compatible API.
- **Your interfaces.** A terminal UI, a web dashboard with live voice, `habakkuk talk`, and a desktop app.

## Documentation

The docs are in [`docs/`](docs/index.md) and at [habakkuk.ai/docs](https://habakkuk.ai/docs).

- **Getting started:** [installation](docs/getting-started/installation.md), [quick start](docs/getting-started/quickstart.md), [configuration](docs/getting-started/configuration.md)
- **Guides:** [channels](docs/guides/channels.md), [WhatsApp](docs/guides/whatsapp.md), [agents](docs/guides/agents.md), [models](docs/guides/models.md), [memory and skills](docs/guides/memory-and-skills.md), [tools and approvals](docs/guides/tools-and-approvals.md), [MCP](docs/guides/mcp.md), [schedules and the board](docs/guides/schedules-and-board.md), [deployment](docs/guides/deployment.md) and [more](docs/index.md#where-to-go-next)
- **Reference:** [CLI](docs/reference/cli.md), [configuration](docs/reference/configuration.md), [HTTP API](docs/reference/api.md), [architecture](docs/reference/architecture.md)

## Project layout

| Path | What is there |
|---|---|
| [`cmd/`](cmd/habakkuk) | The `habakkuk` CLI |
| [`internal/`](internal) | The agent, channels, models, tools, storage and protocols, grouped by area ([map](docs/contributing/codebase.md)) |
| [`apps/desktop`](apps/desktop) | Habakkuk Desktop, a native app around the dashboard (its own Go module) |
| [`docs/`](docs/index.md) | Documentation, published at [habakkuk.ai/docs](https://habakkuk.ai/docs) |
| [`scripts/`](scripts) | `install.sh` |
| [`assets/`](assets/brand) | Logo, icons and social card |

## Security

Safe by default: chat-driven and tainted turns need approval for commands, strangers never get them, skills are pinned and scanned, private files are readable only by you, and nothing phones out unless you turn it on. See [Security model](docs/guides/security.md); report problems as described in [SECURITY.md](SECURITY.md).

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) for the development setup, `make` targets and pull request checklist, and follow the [Code of Conduct](CODE_OF_CONDUCT.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md). Changes are recorded in [CHANGELOG.md](CHANGELOG.md).

## License

Habakkuk's code is licensed under the [Apache License 2.0](LICENSE); see also [NOTICE](NOTICE). The name "Habakkuk" and the logo are trademarks and are not covered by that license: forks must use their own name and logo. See [TRADEMARKS.md](TRADEMARKS.md).
