---
title: Codebase map
description: "A map of cmd/ and internal/: what each package does, where to start reading, and how a message travels from a chat to the model and back."
order: 1
---

Habakkuk is one Go module, `github.com/habakkuk-ai/habakkuk`. The binary is `cmd/habakkuk`; everything else is under `internal/`, grouped by area. The desktop app (`apps/desktop`) is a separate module. For the design behind this layout, read [Architecture](../reference/architecture.md).

## Top level

| Path | What is there |
|---|---|
| `cmd/habakkuk/` | The CLI. `main.go` holds the usage text and the command switch; each command group has its own file (`board.go`, `sessions.go`, `setup.go`, `update.go`, `security.go` ...) |
| `internal/` | The packages below |
| `apps/desktop/` | Habakkuk Desktop, its own Go module (Wails v3, CGO) |
| `docs/` | This documentation |
| `scripts/install.sh` | The release installer |
| `assets/brand/` | Logo, icons and social card |
| `Dockerfile` | The container image, with an optional `browser` target |
| `.github/workflows/` | CI (`ci.yml`) and releases (`release.yml`) |

## `internal/`

Dependencies point downward: `agent` uses `llm`, `tools` and `core/session`; `core/jobs` knows nothing about agents; `core/store` knows nothing above it. Only `app` sees everything.

### `app`

Wires configuration, storage, models, tools, channels and workers together. `app.Open` builds an `App` from `$HABAKKUK_HOME`; `App.Pools` returns the worker pools for each job kind. Files are split by area (`models.go`, `platforms.go`, `media.go`, `board.go`, `nodes.go`, `telemetry.go` ...). The CLI commands call into it.

### `agent`

The turn loop and prompt assembly. `Runner.HandleTurn` (`loop.go`) is the handler for `turn` jobs: it loads and repairs the transcript, calls the model, runs tools, appends results, and repeats. Also here: compaction (`compact.go`), delegation (`delegate.go`, `subturns.go`), identity and the first-chat welcome (`identity.go`), per-session overrides (`overlay.go`), structured output (`schema.go`), taint tracking (`taint.go`), titles, tool search and git worktrees for sub-agents.

### `llm`

Provider-neutral types (`types.go`: `Provider`, `Request`, `Response`, `Block`), error classification (`errors.go`) and the `Router` (`router.go`) that retries, rotates keys and falls back. Adapters: `anthropic`, `openai` (Chat Completions and Responses), `gemini`, `bedrock`. `auth` holds subscription logins; `usage` records tokens and prices them.

### `tools`

The tool `Registry`, toolsets, permission modes and the policy (`tools.go`), built-in core tools (`builtin.go`, `exec.go`, `patch.go`, `search.go`, `process*.go`), the hardline command blocklist, the SSRF guard and taint. Sub-packages are toolsets and helpers:

| Package | What it does |
|---|---|
| `tools/browser` | Browser toolset over the Chrome DevTools Protocol, with the URL guard |
| `tools/computer` | Computer toolset: screenshots, mouse and keyboard through OS commands |
| `tools/meeting` | Meeting attendance through a dedicated Chrome and live captions |
| `tools/homeassistant` | Home Assistant tools and event triggers |
| `tools/media` | Attachment type detection, speech-to-text |
| `tools/mediatools` | Text-to-speech, images, video, `view_image`, `read_document`, `web_search` |
| `tools/voice` | `habakkuk talk`: microphone, voice activity detection, wake phrases, playback |
| `tools/review` | The LLM approval reviewer |
| `tools/sandbox` | Docker and SSH command backends |

### `channel`

The `Gateway` (`gateway.go`) decides who may talk, maps conversations to agents and sessions, handles chat commands (`commands.go`, `session_commands.go`), approvals and clarify questions, and delivers replies. The `Supervisor` (`supervisor.go`) restarts adapters and watches their heartbeat. `channel.go` defines the `Adapter` interface and its optional extras. One sub-package per platform: `telegram`, `slack`, `whatsapp`, `whatsappagent`, `whatsappcloud`, `discord`, `signal`, `matrix`, `email`, `sms`, `msteams`, `googlechat`, `bluebubbles`, `line`, `mattermost`, `irc`, `ntfy`, `messenger` (also Instagram), `feishu`, `dingtalk`, `wecom`, `zulip`, `rocketchat`, `nextcloudtalk`, `twitch`. Shared helpers: `metagraph` (Meta Graph API), `apptoken` (cached app access tokens), `jwt` (RS256 tokens for Bot Framework and Google), `upload` (multipart bodies).

`channel/web` is the HTTP surface: the chat page, the JSON API and Server-Sent Events, the dashboard's API, webhooks and the OpenAI-compatible API. The dashboard's front end is in `channel/web/ui`.

### `core`

| Package | What it does |
|---|---|
| `core/jobs` | The durable queue: `Enqueue`, `Claim` with a lease, `Heartbeat`, `Complete`, serial keys, retries, reclaiming jobs of dead processes; `Pool` runs handlers |
| `core/session` | Sessions and append-only transcripts, crash repair, full-text search |
| `core/state` | Small per-session state: overrides, goals, steering, task lists, progress drafts |
| `core/store` | Opens and migrates the SQLite databases; the blob store; atomic file writes |
| `core/checkpoint` | Workspace snapshots in a shadow git repository |
| `core/events` | The in-process event bus that streams turn activity to clients |
| `core/frontmatter` | YAML frontmatter for Markdown files |

### `knowledge`

| Package | What it does |
|---|---|
| `knowledge/memory` | Memory notes, the FTS5 and embedding index, memory tools, the user profile; `memory/consolidate` runs background review and the nightly pass |
| `knowledge/skills` | `SKILL.md` discovery, precedence, skill tools, the curator |
| `knowledge/hub` | Installs skills and plugins: sources, safe unpacking, scan, tree hash, lock file, backups |
| `knowledge/plugins` | `habakkuk-plugin.yaml` packages: loading, describing, script commands |

### `protocols`

| Package | What it does |
|---|---|
| `protocols/mcp` | MCP client: server connections, tools, OAuth, the catalog |
| `protocols/mcpserver` | Habakkuk as an MCP server |
| `protocols/a2a` | Agent2Agent server and client |
| `protocols/acp` | Agent Client Protocol server for editors |
| `protocols/nodes` | Device nodes: pairing, connections, capabilities |

### `work`, `ops`, `config`, `tui`

| Package | What it does |
|---|---|
| `work/schedule` | Scheduled prompts and scripts with at-most-once slots |
| `work/board` | The task board and its dispatcher |
| `ops/hooks` | User commands run on events |
| `ops/i18n` | Message catalogs (YAML per locale in `locales/`) |
| `ops/logs` | Log rotation, redaction and reading |
| `ops/secrets` | Secret manager sources |
| `ops/security` | `habakkuk security audit` |
| `ops/telemetry` | Metrics and OTLP traces |
| `config` | Strict YAML config: types, defaults, validation, provider presets, the starter config |
| `tui` | The full-screen terminal UI (Bubble Tea) over a narrow `Backend` interface |

## Main flows

### A chat message

```
platform ──> adapter ──> supervisor ──> gateway ──> turn job ──> agent
                ^                                       │
                └──────── delivery job <────────────────┘
```

1. An adapter (`internal/channel/<type>`) receives a message and hands it to the inbox; the `Supervisor` keeps the adapter running.
2. `Gateway.Handle` checks the sender (allowlist or pairing), whether a group message is addressed to the bot, and handles commands. Otherwise it picks the agent and session, appends the message and enqueues a `turn` job whose ID derives from the platform message ID.
3. A worker in a `jobs.Pool` claims the job with a lease. `Gateway.HandleTurn` wraps `agent.Runner.HandleTurn`, which runs the loop: `llm.Router.Send` streams the model call, `tools.Registry.Execute` runs tool calls under the permission policy, and every append checks the lease.
4. When the turn finishes, the gateway enqueues a delivery job; `Gateway.HandleDeliver` formats the reply, splits it into chunks, and records each chunk sent so a retry resumes where it stopped.

### Other entry points

- **CLI and TUI** (`habakkuk chat`, `run`, `tui`) create a session and run the same turn jobs in-process (`Runner.Ask`, `Begin`, `Await`), following live events from `core/events`.
- **HTTP** (`channel/web`): the web chat is an ordinary adapter; webhooks, the OpenAI-compatible API, A2A and MCP over HTTP start turns through the same runner.
- **Schedules and the board** (`work/`) enqueue jobs whose IDs derive from the slot or the card attempt, so each runs at most once.
- **Delegation** enqueues `subturn` jobs that run in their own pool.

### Crash recovery

On start, `jobs.Queue.ReleaseDead` frees jobs whose owner process is gone; they are claimed again and the turn loop repairs the transcript (a tool call without a result gets an "interrupted" result) before going on.

## Where to start reading

1. `internal/core/jobs/queue.go` and `worker.go`: the queue everything runs on.
2. `internal/agent/loop.go`: one turn.
3. `internal/channel/gateway.go`: from a chat message to a turn and back.
4. `internal/app/app.go`: how it is all put together.
5. `cmd/habakkuk/main.go`: the commands.

Tests sit beside the code (`*_test.go`); adapters are tested against fake platform servers, and the job queue and turn loop include crash and retry cases.
