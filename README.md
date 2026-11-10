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

On Windows (PowerShell):

```powershell
irm https://habakkuk.ai/install.ps1 | iex
```

The scripts check the release against its `SHA256SUMS` and install `habakkuk` in `~/.local/bin` (`%LOCALAPPDATA%\Programs\Habakkuk` on Windows), with `hk` beside it as the short name: `hk chat` is `habakkuk chat`. The same scripts are in [`scripts/`](scripts). You can also use `go install github.com/habakkuk-ai/habakkuk/cmd/habakkuk@latest` or the container image `ghcr.io/habakkuk-ai/habakkuk`; see [Installation](docs/get-started/installation.md).

## Quick start

Type `hk` on its own. On a new computer it prints one short screen that ends with the command to run next:

```bash
hk setup        # one-minute wizard: model, WhatsApp/Telegram/Slack/..., names
```

Setup finds the API keys and local models on your computer, connects the app you want to talk in, checks that your agent answers, and offers to keep it running. No YAML to edit. After that:

```bash
hk chat         # talk to it in the terminal
hk serve        # run channels, schedules, the board and the dashboard
hk doctor       # check config, keys and channels; each problem comes with its fix
hk status       # what is running, the queue, today's cost
hk -r srv chat  # the same commands, on the Habakkuk on your server
```

`hk` and `habakkuk` are the same program. The [quickstart](docs/get-started/quickstart.md) walks through the first run.

Coming from another agent? `habakkuk setup` offers to import it on a new install, or run `habakkuk migrate --dry-run` to see what would move. See [Move from another agent](docs/get-started/migrate.md).

## Features

- **Crash-proof work.** Leased jobs, transcript repair, exactly-once replies and resumable deliveries.
- **Many channels.** Telegram, Slack, WhatsApp (linked device, WhatsApp agents, Business Cloud API), Discord, Signal, Matrix, email, SMS, Teams, Google Chat, iMessage, LINE, Mattermost, IRC, ntfy, Messenger, Instagram, Feishu, DingTalk, WeCom, Zulip, Rocket.Chat, Nextcloud Talk, Twitch and a web chat. The channels page says [which of these have run against the real service](docs/channels/index.md#what-has-been-tested-live) so far.
- **One conversation, whoever spoke.** What reaches a chat from elsewhere (a scheduled result, another agent's report, `hk send`) is kept as a chat note and shown to the agent on your next message, so a reply such as "yes" is read against the right thing.
- **Any model, and the right one for each message.** Claude, GPT, Gemini, DeepSeek, Grok, Mistral, Qwen, Kimi, GLM, Bedrock, OpenRouter and 20 more presets, or a local model through Ollama, LM Studio, vLLM or llama.cpp. Quick messages go to a cheap model and real work to a strong one, automatically. Key rotation, fallbacks, subscription logins, spending caps and cost tracking.
- **Tools with guardrails.** Permission modes, an approval reviewer, sandboxes, workspace checkpoints, a browser, computer use and meeting notes.
- **Memory and skills.** Markdown notes with semantic search, a user profile, `SKILL.md` skills and plugins.
- **Teams of agents.** Several agents, delegation, schedules and a shared task board.
- **Retros.** An agent looks back over its own work, finds what went wrong and why, proposes changes, and checks next time whether they helped.
- **Open protocols.** MCP client and server, Agent2Agent, the Agent Client Protocol and an OpenAI-compatible API.
- **Your interfaces.** A terminal UI, a web dashboard with live voice, `hk talk`, and a desktop app.
- **One CLI, here or there.** `hk remote add srv ssh://you@server` saves a server once; then `hk -r srv COMMAND` runs any command on the Habakkuk there.

## Documentation

The docs are in [`docs/`](docs/index.md) and at [habakkuk.ai/docs](https://habakkuk.ai/docs).

- **Get started:** [quickstart](docs/get-started/quickstart.md), [installation](docs/get-started/installation.md), [configuration](docs/get-started/configuration.md), [use cases](docs/get-started/use-cases.md)
- **Channels:** [overview](docs/channels/index.md), [WhatsApp](docs/channels/whatsapp.md), [Telegram](docs/channels/telegram.md), [Slack](docs/channels/slack.md), [Discord](docs/channels/discord.md) and [the rest](docs/channels/index.md#all-channels)
- **Agents:** [agents and identity](docs/agents/agents.md), [multi-agent teams](docs/agents/multi-agent.md), [memory](docs/agents/memory.md), [skills](docs/agents/skills.md), [schedules](docs/agents/schedules.md), [the board](docs/agents/board.md), [retros](docs/agents/retros.md), [permissions and approvals](docs/agents/approvals.md)
- **Models and tools:** [providers](docs/models/providers.md), [routing](docs/models/routing.md), [tools](docs/tools/index.md), [MCP](docs/tools/mcp.md), [plugins](docs/tools/plugins.md)
- **Interfaces:** [CLI and terminal UI](docs/interfaces/cli-and-tui.md), [dashboard](docs/interfaces/dashboard.md), [desktop app](docs/interfaces/desktop-app.md), [remote CLI](docs/interfaces/remote-cli.md)
- **Run it:** [on a server](docs/deploy/server.md), [updating](docs/get-started/updating.md), [security](docs/deploy/security.md), [FAQ](docs/help/faq.md), [troubleshooting](docs/help/troubleshooting.md)
- **Reference:** [CLI](docs/reference/cli.md), [configuration](docs/reference/configuration.md), [HTTP API](docs/reference/api.md), [architecture](docs/reference/architecture.md)

## Project layout

| Path | What is there |
|---|---|
| [`cmd/`](cmd/habakkuk) | The `habakkuk` CLI |
| [`internal/`](internal) | The agent, channels, models, tools, storage and protocols, grouped by area ([map](docs/contributing/codebase.md)) |
| [`apps/desktop`](apps/desktop) | Habakkuk Desktop, a native app around the dashboard (its own Go module) |
| [`docs/`](docs/index.md) | Documentation, published at [habakkuk.ai/docs](https://habakkuk.ai/docs) |
| [`scripts/`](scripts) | The installers: `install.sh` and `install.ps1` |
| [`assets/`](assets/brand) | Logo, icons and social card |

## Security

Safe by default: chat-driven and tainted turns need approval for commands, strangers never get them, skills are pinned and scanned, private files are readable only by you, and nothing phones out unless you turn it on. See [Security](docs/deploy/security.md); report problems as described in [SECURITY.md](SECURITY.md).

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) for the development setup, `make` targets and pull request checklist, and follow the [Code of Conduct](CODE_OF_CONDUCT.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md). Changes are recorded in [CHANGELOG.md](CHANGELOG.md).

## License

Habakkuk's code is licensed under the [Apache License 2.0](LICENSE); see also [NOTICE](NOTICE). The name "Habakkuk" and the logo are trademarks and are not covered by that license: forks must use their own name and logo. See [TRADEMARKS.md](TRADEMARKS.md).
