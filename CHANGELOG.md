# Changelog

All notable changes to Habakkuk are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Version in development: `0.3.0-dev`. No release has been tagged yet; this section summarises what the code does today.

### Added

- **Durable core.** Every turn is a job with a lease and heartbeat in a SQLite queue; a process killed at any point loses no committed work, and a restart reclaims jobs whose owner died. Interrupted tool calls are repaired, not repeated. Storage is split into `control.db`, `transcripts.db`, `state.db`, `web.db`, `usage.db`, `nodes.db` and content-addressed blobs, with retention on by default.
- **Decision model.** An optional `judge:` (TypeSafe's Jev) makes Habakkuk's own small decisions: which model a message needs, whether a tool call is plainly routine before the reviewer model is asked (never for a turn that read outside content), and whether a chat is worth a memory review. Without a key, or when it is slow, everything works as before.
- **Model routing.** An agent's `fast_model` takes quick messages and chores (titles, summaries, memory review) while `model` takes real work; long tool loops move up to `model` automatically (`routing: auto | fast | smart`, `escalate_after`). `habakkuk setup` pairs each provider's recommended model with its cheaper one.
- **Models.** Providers of type `anthropic`, `openai` (any OpenAI-compatible API), `responses`, `gemini` and `bedrock`; presets for well-known services and local servers; retry with backoff, key rotation and fallback down a model chain; ChatGPT and GitHub Copilot subscription logins; per-session `/model`; token usage and cost in `usage.db` (`habakkuk usage`, `/usage`).
- **Channels.** Telegram, Slack (Socket Mode, many workspaces), WhatsApp as a linked device, WhatsApp agents, WhatsApp Business Cloud API, Discord, Signal, Matrix, email (IMAP and SMTP), SMS (Twilio), Microsoft Teams, Google Chat, iMessage (BlueBubbles), LINE, Mattermost, IRC, ntfy, Messenger, Instagram, Feishu/Lark, DingTalk, WeCom, Zulip, Rocket.Chat, Nextcloud Talk, Twitch and a web chat. Each runs under a supervisor with a heartbeat watchdog; pairing codes and allowlists; idempotent turns and resumable deliveries; chat approvals, buttons, reactions, polls and live progress drafts.
- **Agents.** Several agents with their own `SOUL.md`, `AGENTS.md` and `IDENTITY.md`; a first-chat welcome that lets the user name the agent; permission modes (`read-only`, `ask`, `workspace`, `full`); an LLM approval reviewer; compaction of long sessions; delegation to sub-agents with structured output, git worktrees and background runs; tool search for large tool catalogs; session titles, fork, rewind and export.
- **Tools.** Files, `exec`, `apply_patch`, `search_files`, background processes, Docker and SSH sandboxes, workspace checkpoints with `habakkuk rollback` and `/undo`, a hardline command blocklist and an SSRF guard; a browser toolset over the Chrome DevTools Protocol; a computer toolset (Linux, macOS, per-agent Xvfb); meeting attendance on Google Meet, Zoom and Teams; images, video, documents, web search and speech.
- **Memory and skills.** Markdown memory notes with full-text and optional semantic search, a user profile (`USER.md`), provenance and untrusted marking, background review and nightly consolidation; `SKILL.md` skills with `/skill` and `/learn`; installing skills and plugins from GitHub, URLs, paths and registries with a lock file, a security scan and backups.
- **Work.** Schedules (cron, `@every`, one-off) with at-most-once slots and script schedules; a task board shared by agents.
- **Protocols.** MCP client (stdio and Streamable HTTP, OAuth sign-in, a catalog of servers); Habakkuk as an MCP server; Agent2Agent server and client (1.0 and 0.3); Agent Client Protocol for code editors; an OpenAI-compatible API; Home Assistant tools and triggers; device nodes that lend capabilities from other machines and browsers.
- **Interfaces.** The CLI, a full-screen terminal UI, a web dashboard at `/app/` with a JSON API, Live voice mode over WebRTC, `habakkuk talk` for a local voice loop, and Habakkuk Desktop for macOS, Windows and Linux.
- **Operations.** `habakkuk setup`, `doctor`, `status`, `logs`, `security audit`, `expose tailscale`, `backup`, `restore` and `update`; Prometheus metrics, OTLP traces, log rotation with secret redaction, secret managers, hooks, and localized chat and CLI text.
- **Releases.** Static binaries for Linux and macOS (amd64, arm64) with `SHA256SUMS`, a multi-arch container image on ghcr.io, desktop app builds, and install scripts for macOS, Linux and Windows (`curl -fsSL https://habakkuk.ai/install.sh | sh`).
- **Project.** Contributing guide, code of conduct, security policy, `NOTICE`, Apache License 2.0, trademark policy (`TRADEMARKS.md`), brand guide, issue and pull request templates, Dependabot, a Makefile, and a docs tree under `docs/`.

[Unreleased]: https://github.com/habakkuk-ai/habakkuk/commits/main
