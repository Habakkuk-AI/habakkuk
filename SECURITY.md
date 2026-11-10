# Security policy

Habakkuk runs commands, reads files, holds API keys and chat credentials, and talks to the network on your behalf, so security reports are welcome and taken seriously.

## Reporting a vulnerability

Please do not open a public issue, pull request or discussion for a vulnerability. Report it privately, either way:

- **GitHub:** open a private report under [Security > Report a vulnerability](https://github.com/habakkuk-ai/habakkuk/security/advisories/new) (GitHub security advisories).
- **Email:** security@habakkuk.ai.

Include what you found, the version or commit, your configuration (with secrets removed), and the steps or a proof of concept that shows the problem. You will get an acknowledgement, and updates as the report is investigated and fixed. Once a fix is released, the advisory is published, with credit to you unless you prefer otherwise.

## Supported versions

Habakkuk is before 1.0. Fixes go into the `main` branch and the next release; older releases are not patched.

| Version | Supported |
|---|---|
| `main` and the latest release | yes |
| Older releases | no; update with `hk update` |

## Scope

In scope: the code in this repository.

- The `habakkuk` binary and everything under `cmd/` and `internal/`: the permission modes and approval flow, the approval reviewer, the hardline command blocklist, the SSRF guard on `fetch_url` and the browser's URL guard, sandboxes, the HTTP server and its token checks, webhook and channel signature checks, pairing and allowlists, device node pairing, MCP OAuth, secret redaction in logs, memory and profile provenance (trusted and untrusted content), skill and plugin installation and scanning, backups and restore.
- The desktop app in `apps/desktop`: its loopback server and proxy, session cookie, and token storage.
- The installers (`scripts/install.sh`, `scripts/install.ps1`), `hk update`, and the release workflow (checksums, the `SHA256SUMS` signature, published artifacts, the container image).

Out of scope:

- A model doing something harmful when you allowed it: `full` permission mode, an agent that outsiders can reach with `approvals: reviewer`, a human approving a dangerous call, or an MCP server or plugin you installed and enabled. Bypassing a guard that should have stopped it is in scope.
- Prompt injection that leads only to a wrong or misleading answer, with no action, data leak or privilege gain.
- Vulnerabilities in third-party services, model providers, chat platforms or MCP servers, unless Habakkuk's use of them makes the problem worse.
- Problems that need an attacker who already controls your account, your machine or `$HABAKKUK_HOME`.
- Known gaps listed in the docs, such as the browser URL guard not intercepting WebSocket handshakes or service workers (see [docs/reference/architecture.md](docs/reference/architecture.md)), unless you find a way to make them worse.

`hk security audit` checks a running setup for risky settings; it is a good first step before reporting a configuration problem.
