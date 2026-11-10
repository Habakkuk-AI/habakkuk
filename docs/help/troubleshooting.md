---
title: Troubleshooting
description: "Common problems by symptom, with the exact message Habakkuk prints and how to fix it: config, keys, channels, approvals, budgets, updates and the HTTP server."
order: 2
---

Something not working? This page lists common problems by what you see, with the cause and the fix for each.

Before you look one up, run `hk doctor`. It checks the config, every provider's keys, the databases, each channel's credentials, the HTTP section and MCP servers. It prints one line per item and exits non-zero when something is wrong.

Some problems have a fix that needs no decision from you. Run `hk doctor --fix` and it makes those fixes first, says what it changed, and then runs the usual checks:

- An older `config.yaml` is rewritten in the current format. The old file is kept as `config.yaml.<time>.bak`.
- Folders Habakkuk needs inside `$HABAKKUK_HOME` and can't find, such as an agent's folder or its default workspace, are created. A workspace elsewhere isn't: if it's missing, the drive it's on may not be mounted.
- Files and folders under `$HABAKKUK_HOME` that other users can read are made owner-only, and skills and backups in it lose the write or read bits others had, as `hk security -fix` does.
- A background service whose program no longer exists (you moved the binary) is pointed at the binary you just ran. Only the program's path changes; your other settings in the file stay. The old file is copied to `$HABAKKUK_HOME/backups/` with the time in its name, and the fix tells you the command that reloads the service. A service for another home, or a fix that would point at a temporary `go run` build, is left alone.

Anything that needs a key, a login, a choice or a change outside `$HABAKKUK_HOME`, such as a hook script other users can change, stays as advice. A plain `hk doctor` ends with the list of what `--fix` would do.

After that, run `hk channels` for live channel health, and `hk logs -f -level warn` to see what `serve` is doing.

If the problem is with one chat app, look at its channel page too. Each one has its own troubleshooting table: [WhatsApp](../channels/whatsapp.md), [Telegram](../channels/telegram.md#troubleshooting), [Slack](../channels/slack.md#troubleshooting), [Discord](../channels/discord.md#troubleshooting), [Signal](../channels/signal.md#troubleshooting), [Matrix](../channels/matrix.md#troubleshooting).

## Setup and config

| You see | Cause and fix |
|---|---|
| ``no config at ~/.habakkuk/config.yaml yet: run `hk setup` `` | No config yet. Run `hk setup` (or `hk init` to write a starter file by hand). If you have one elsewhere, set `HABAKKUK_HOME`. |
| `config: UNKNOWN KEY "modle" in Agent (line 12) (ignored; ...)` | A typo, or a setting from a newer release. Fix the spelling; the key is ignored until then. |
| `agent habakkuk: unknown provider "x" in "x/model"` | The part before `/` in `model` must be a name under `providers:`. |
| `model "x": want <provider>/<model>` | Write models as `provider/model-id`. |
| `agent habakkuk: routing "auto" needs a fast_model` | Add a `fast_model`, or remove `routing`. |
| `agent habakkuk: toolset "mcp:x" names no configured MCP server` | Add the server under `mcp_servers:`, or fix the name. |
| `channel telegram: token_env is required for telegram` | Each channel type has required settings; see its page. |
| `channel x: agent "y" is not configured` | The channel's `agent` (or a route) names an agent that does not exist. |
| `transcription: provider "x" must be a configured provider of type openai` | Point `transcription.provider` at a provider with `type: openai` (or an OpenAI-compatible preset). |
| `version N is not supported (want M)` | The config was written for a newer release. Update Habakkuk, or put back the config that came with this one (`hk update --rollback` restores it). |
| A hand edit to `config.yaml` seems ignored | `serve` logs `config.yaml changed; restart hk serve to apply` for most sections, and `config.yaml changed but does not load; still running the previous config` when the file is invalid. Fix it, then restart `serve`. |

## Keys and providers

| You see | Cause and fix |
|---|---|
| `provider anthropic: 0/1 keys present` in `hk doctor` | The variable in `keys_env` is not set. Put it in `$HABAKKUK_HOME/.env` (not in `config.yaml`). |
| `provider chatgpt: NOT LOGGED IN (run hk auth login chatgpt)` | Run the command it names. |
| Turns fail with an auth or billing error | The key is wrong, revoked or out of credit. With several keys in `keys_env`, Habakkuk rotates to the next one; with `fallbacks`, it tries the next model. |
| `mcp x: MISSING ...` or `not signed in; run hk mcp login x` | Set the variable the server needs, or sign in. |

## Channels

| Symptom | Cause and fix |
|---|---|
| The bot answers with a pairing code | You are not in `allow_from`. Run `hk pair CODE` on the server (the code works for one hour). |
| The bot ignores group messages | By default it answers only when mentioned or replied to (`groups: mention`). Set `groups: all` to answer everything. |
| Nothing happens, and another `hk serve` is running | Only one program may poll a channel. Stop the other one; run one `serve` per `$HABAKKUK_HOME`. |
| A webhook channel (SMS, Teams, Google Chat...) receives nothing | The platform must reach `<public_url>/channels/<name>`. Check `public_url`, the proxy or tunnel, and the URL `hk doctor` prints. Tailscale addresses are private, so platforms cannot reach them. |
| A channel shows as down in `hk channels` | It is being restarted with backoff. Its last error is shown; errors marked fatal (a bad token) stop it until you fix the config and restart `serve`. |
| Voice notes are not understood | Configure [`transcription:`](../models/speech.md#speech-to-text). Without it, the agent is told it cannot hear the note. |
| `This chat can't receive files.` on `/export` | The platform has no file sending in Habakkuk (for example IRC or Twitch). Use `hk sessions export ID`. |

## Approvals and permissions

| You see | Cause and fix |
|---|---|
| `No answer, so I skipped that step.` | An approval waited 15 minutes. Answer **yes** or **no** sooner, or use an [approval reviewer](../agents/approvals.md#approvals) for routine steps. |
| `... needs approval and no one is attending this run` | A schedule, sub-agent, webhook or API call needed approval. Give the agent `approvals: reviewer` or `reviewer_then_human`, or a wider `permission`. |
| `... is not allowed in read-only mode` | The agent is `permission: read-only`, which also denies network tools such as the browser. Use `workspace`. |
| `... is not available to messages from people other than the owner` | The sender is a guest (let in by `allow_from: ["*"]`). Add their id to `allow_from` or pair them. |
| Commands need approval even in `full` mode | Turns from chat apps and turns that read outside content always need approval for risky calls. Set `trust: full` on the agent to treat your own chat messages like the CLI. See [Security](../deploy/security.md). |

## Budgets

| You see | Cause and fix |
|---|---|
| `Paused: the daily budget of $5.00 for agent habakkuk is used up; it resets at 00:00 ...` | A [spending limit](../models/usage-and-limits.md#spending-limits) was reached. Raise it in `config.yaml` (it applies within seconds) or wait for the reset. |
| `Paused: x has no known price and budget.unknown_price is block` | Add the model under `pricing:`, or set `unknown_price: allow`. |

## Updates and databases

| You see | Cause and fix |
|---|---|
| `control.db has schema version N, but this habakkuk only knows up to version M` | A newer Habakkuk already upgraded the database. Run `hk update`, or restore with `hk update --rollback`. |
| `hk update` stops after the upgrade check | The new release cannot run on your home as it is; nothing was changed. Read the message, or pass `-skip-upgrade-check` only if you are sure. |
| Something broke after an update | Stop `serve` and run `hk update --rollback`. |
| `service: ... runs /path/to/habakkuk, which no longer exists` | The background service still points at an old copy of the binary. Run `hk doctor --fix`, then the reload command it prints. |

## HTTP server, dashboard and desktop app

| You see | Cause and fix |
|---|---|
| `http: listen 0.0.0.0:8484 is not a loopback address; set token_env so the API needs a token` | Set `http.token_env`, or listen on `127.0.0.1`. |
| `a channel of type web needs the http section` | Add an `http` section. |
| `no http section in config.yaml` from `hk expose`, or the desktop app says `config.yaml has no http section` | Add `http: {listen: 127.0.0.1:8484, token_env: HABAKKUK_HTTP_TOKEN}` and put the token in `.env`. |
| `http.token_env is not set: give the API a token before sharing it on the tailnet` | Set `token_env` first. |
| Live mode cannot use the microphone | The page needs `localhost` or HTTPS. Use `hk expose tailscale` or a reverse proxy with HTTPS. |

## Tools

| You see | Cause and fix |
|---|---|
| `computer use is not supported on Windows yet` | Use the browser tools, or run Habakkuk in WSL. |
| The browser does not start on a server | Install Chromium (`apt install chromium` on Debian), or set `browser.path`. In Docker, build the image with `--target browser`. |
| `duckduckgo refused the automated search` | Configure a search provider with a key (Brave, Tavily, Exa) or your own SearXNG. |
| Checkpoints are missing | `git` must be on `PATH`, and snapshots larger than `checkpoint_max_mb` are skipped. |
| `background services are set up for Linux (systemd) and macOS (launchd)` | `hk setup service` does not cover your OS; run `hk serve` with your own service manager. |
