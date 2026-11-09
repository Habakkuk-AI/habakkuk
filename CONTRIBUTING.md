# Contributing to Habakkuk

Thanks for helping. This page covers the development setup, the layout of the code, how to add the most common kinds of change, and what a pull request needs.

By taking part you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md). Report security problems privately, as described in [SECURITY.md](SECURITY.md), not in public issues.

## Development setup

You need:

- **Go 1.27** (the exact version is in `go.mod`). The `habakkuk` binary builds without CGO.
- **git**, and **make** for the shortcuts below.
- [golangci-lint](https://golangci-lint.run) v2 for `make lint` (CI uses v2.15.0 with `.golangci.yml`).
- Optional: [shellcheck](https://www.shellcheck.net) for `scripts/install.sh`; a C toolchain and the platform webview for the desktop app (see [docs/interfaces/desktop-app.md](docs/interfaces/desktop-app.md#building)); Node.js and pnpm for the website.

```bash
git clone git@github.com:habakkuk-ai/habakkuk.git
cd habakkuk
make build        # bin/habakkuk
make test         # go test ./...
```

To try a build without touching your own setup, point `HABAKKUK_HOME` at a scratch folder:

```bash
export HABAKKUK_HOME=$PWD/.habakkuk-dev
bin/habakkuk init
echo 'ANTHROPIC_API_KEY=...' >> .habakkuk-dev/.env
bin/habakkuk doctor
bin/habakkuk chat
```

### Make targets

| Target | What it does |
|---|---|
| `make build` | Build `bin/habakkuk` from `./cmd/habakkuk` with `CGO_ENABLED=0` |
| `make test` | `go test ./...` |
| `make race` | `go test -race ./...` (what CI runs) |
| `make lint` | `golangci-lint run ./...` |
| `make fmt` | `gofmt -w` over `cmd` and `internal` |
| `make vet` | `go vet ./...` |
| `make run ARGS="chat"` | Build, then run `bin/habakkuk` with `ARGS` (default `chat`) |
| `make desktop` | Build the desktop app in `apps/desktop` (its own Go module; needs CGO and a webview) |
| `make clean` | Remove `bin/` and `dist/` |
| `make help` | List the targets |

`go test ./...` at the root does not include `apps/desktop`, which is a separate module; run its tests with `cd apps/desktop && go test ./...`.

## Project layout

| Path | What is there |
|---|---|
| `cmd/habakkuk/` | The CLI: one `main.go` with the command switch and a file per command group |
| `internal/` | Everything else, grouped by area (below) |
| `apps/desktop/` | Habakkuk Desktop, a separate Go module (Wails v3) |
| `docs/` | The documentation (Markdown with `title`, `description` and `order` frontmatter), published at habakkuk.ai/docs |
| `scripts/` | `install.sh`, the release installer |
| `assets/brand/` | Logo, mark, icons and social card |

The `internal/` groups:

| Folder | Area |
|---|---|
| `agent` | The turn loop, prompt assembly, compaction, delegation |
| `app` | Wires config, stores, providers, tools, channels and workers together |
| `channel` | The gateway, supervisor and formatting, plus one sub-package per chat platform |
| `config` | Strict YAML config: types, defaults, validation, provider presets |
| `core` | Durable jobs, sessions, state, storage, checkpoints, events, frontmatter |
| `knowledge` | Memory, skills, the skill and plugin installer, plugins |
| `llm` | Provider-neutral types, the router, and the provider adapters |
| `ops` | Hooks, localization, logs, secrets, the security audit, telemetry |
| `protocols` | MCP client and server, Agent2Agent, Agent Client Protocol, device nodes |
| `tools` | The tool registry and permission policy, built-in tools and toolsets |
| `tui` | The full-screen terminal UI |
| `work` | Schedules and the task board |

[docs/contributing/codebase.md](docs/contributing/codebase.md) maps every package and walks through the main flows. [docs/reference/architecture.md](docs/reference/architecture.md) explains the design in depth.

Dependencies point downward: `agent` uses `llm`, `tools` and `core/session`; `core/jobs` knows nothing about agents; `core/store` knows nothing above it. Keep it that way.

## Common changes

### Add a channel

1. Create `internal/channel/<type>/` with an adapter that implements `channel.Adapter` (`Run`, `Send`, `Format`, `MaxLen`, `StaleAfter` in `internal/channel/channel.go`). Call the inbox's beat on every message and keepalive, so the supervisor can tell a hung connection from a quiet one. Implement the optional interfaces the platform supports: `Typer`, `FileSender`, `Editor`, `ButtonSender`, `Reactor`, `Poller`, `VoiceSender`.
2. Add the type's settings. Simple types use the fields on `config.Channel`; newer ones get a block named after the type in `internal/config/platforms.go` or `platforms2.go`, with validation beside it. Secrets are always named by an `*_env` field, never stored in the config.
3. Build the adapter in `internal/app` (`app.go`, `platforms.go` or `platforms2.go`). Webhook adapters are mounted at `/channels/<account>` and must check every request's signature themselves.
4. Add tests with a fake platform server (see the existing adapters' `_test.go` files), and document it: a row in the catalog in [docs/channels/index.md](docs/channels/index.md) and in the support table in [docs/channels/settings.md](docs/channels/settings.md), and a page of its own in `docs/channels/` (or a section in `more-platforms.md` for a small platform).

### Add a tool

1. Implement `tools.Tool` (`Spec`, `Risk`, `Run` in `internal/tools/tools.go`). Pick the narrowest `Risk` (`read`, `write`, `network`, `exec`): the permission policy decides from it. Implement `PathTool` or `PathsTool` when the tool acts on paths, so workspace mode can allow writes inside the workspace.
2. Register it in a toolset: core tools in `RegisterBuiltins` (`internal/tools/builtin.go`), a new toolset in its own package with a `RegisterTools(r *tools.Registry, ...)` function called from `internal/app`.
3. Treat anything read from outside (web pages, files from a chat, API responses) as data, not instructions. A tool that reads outside content should have `network` risk: the registry marks the turn as tainted after a `network` tool runs, which keeps what it read out of trusted memory and the user profile.
4. Test `Run` directly and through the registry's permission checks.

### Add a model provider

- **An OpenAI-compatible service** needs no code beyond a preset: add an entry to `Presets` in `internal/config/providers.go` (type, base URL, key variable, whether the key is optional), a test in `providers_test.go`, and a row in the provider directory in [docs/models/index.md](docs/models/index.md).
- **A new API** needs an adapter in `internal/llm/<name>/` that implements `llm.Provider` (`Stream` in `internal/llm/types.go`) and maps its errors onto the router's classes (rate limit, overload, auth, billing, context overflow, bad request, network), so retry, key rotation and fallback work. Disable the SDK's own retries. Wire the new `type` in `internal/app/models.go` and accept it in config validation.
- Add list prices for its models to the built-in pricing table in `internal/llm/usage/pricing.go` when they are published.

## Commit style

Commits follow [Conventional Commits](https://www.conventionalcommits.org): `type(scope): summary` in the imperative, lower case, no trailing period.

- Types: `feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`.
- Scope is the area touched, usually an `internal/` group or a channel: `feat(channel): ...`, `fix(llm): ...`, `docs(guides): ...`.
- Explain in the body what changed and why when the summary is not enough.

Examples from the history: `feat(meeting): attend Meet, Zoom and Teams calls and send notes`, `chore(brand): add the Habakkuk logo, icons and social card`.

## Pull requests

Open an issue first for large changes, so the design can be agreed before the work. Keep a pull request to one change.

Before you open it:

- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes
- [ ] `golangci-lint run` passes
- [ ] `gofmt -l cmd internal` prints nothing for the files you changed
- [ ] New behaviour has tests, including the crash and retry cases where the change touches jobs, deliveries or storage
- [ ] Docs under `docs/` describe the change, and only what the code does
- [ ] No secrets, tokens or personal data in code, tests, fixtures or logs

CI (`.github/workflows/ci.yml`) runs vet, the race tests, shellcheck on `scripts/install.sh`, golangci-lint, and the desktop app's vet and tests.

## Documentation

The docs live in `docs/` and are published at [habakkuk.ai/docs](https://habakkuk.ai/docs). Each folder is a sidebar section (`get-started`, `channels`, `agents`, `models`, `tools`, `interfaces`, `deploy`, `reference`, `help`, `contributing`), and `docs/index.md` is the docs home. Describe what the code does today, and check the code when unsure.

Every page starts with YAML frontmatter:

```yaml
---
title: Quickstart
nav_title: Quickstart          # optional: a shorter sidebar label
description: One sentence for cards, search results and link previews.
order: 1
---
```

- `order` sorts pages within their folder. Each folder's `index.md` is its overview; its `title` is the section's sidebar label and its `order` the section's position.
- Link between pages with relative paths (`../tools/mcp.md`, anchors allowed).
- Callouts use GitHub's alert syntax: `> [!NOTE]`, `> [!TIP]`, `> [!IMPORTANT]`, `> [!WARNING]`, `> [!CAUTION]`. Use them for prerequisites, gotchas and security, not on every paragraph.
- Tabs are consecutive fenced code blocks with `tab="Label"` in the info string, such as ` ```bash tab="macOS / Linux" ` followed by ` ```powershell tab="Windows" `.
- Card lists are an HTML comment `<!-- cards -->` on its own line, directly before a list whose items are `- [Title](path.md): one-line description`.
- When you move or rename a page, add the old path to `docs/redirects.json`.

## License

By contributing you agree that your contributions are licensed under the [Apache License 2.0](LICENSE), as described in its section 5. The Habakkuk name and logo are not covered by that license; see [TRADEMARKS.md](TRADEMARKS.md).
