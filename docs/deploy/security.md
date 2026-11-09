---
title: Security
description: "What Habakkuk lets an agent do by default: approvals for chat-driven and tainted turns, guests, vetted skills, private files, no surprise network calls, a locked-down local server, the audit and secret managers."
order: 3
---

Habakkuk is safe by default. `habakkuk security` shows what it allows on your machine: the effective tool policy of each agent, then an audit with fixes. `habakkuk status` shows the policy too.

```bash
habakkuk security             # tool policy per agent, then the audit
habakkuk security -fix        # also apply the safe fixes (file permissions)
habakkuk security -network    # every outbound endpoint, and whether your config turns it on
```

## Risky capabilities

Five kinds of tool call can hurt the machine Habakkuk runs on:

| Capability | What it covers |
|---|---|
| `exec` | The shell, and MCP servers marked `risk: exec` |
| `process` | Input to and killing background processes |
| `write` | Files outside the agent's workspace |
| `computer` | Screen, mouse and keyboard, and a paired device's screen, camera and clipboard |
| `browser` | Pages on loopback or private addresses that `browser.allow_private` lets through |

## Chat channels are least-privilege

A turn that comes from a messaging channel (anything but the CLI, the TUI and the local dashboard) needs approval for every risky capability, whatever the permission mode, `full` included. You approve in the chat. To let your own chat messages act like the CLI, opt in per agent:

```yaml
agents:
  habakkuk:
    trust: full               # or lift only some: channel_allow: [write, browser]
```

## Strangers never get them

A sender let in by `allow_from: ["*"]` (rather than by id or `habakkuk pair`) is a **guest**. Guests' turns are refused every risky capability, with or without approval. Guests cannot answer your pending approvals, and cannot use `/remember`, `/forget` or `/agent`. A turn with no sender behind it (a schedule) on an account open to everyone counts as a guest's.

One exception: a `whatsappagent` channel only ever hears from the person who created the agent (WhatsApp refuses everyone else), so its sender is always treated as the owner, even with `allow_from: "*"`.

## Outside content taints a turn

Once a turn has read outside content (a web page, a search result, an MCP or other network tool, an email, a webhook or Home Assistant payload, a device's output), `exec`, `process` and writes outside the workspace need a human's approval. That holds even in `full` mode and even with `approvals: reviewer`: a model is not asked, since it could be steered by the same text. With nobody attending, such calls are denied.

A tainted turn also cannot write trusted memory, change the user profile or set the agent's identity, and `message_home` sends only a short status. A delegated task inherits its parent's taint and origin.

`allow_tainted_exec: true` on an agent turns this off; the audit flags it.

## Skills and plugins from outside

Outside skills and plugins are pinned and checked. An install prints the source, the commit, the tree hash, every file and the scripts among them, and scans them. A high-risk finding (`curl | sh`, decoded payloads run, credentials or the environment sent out, obfuscation, program binaries) refuses the install without `-force`. The lock file records the commit and hash. Updates show a diff and apply only when you say yes.

A skill that ships scripts stays disabled until `habakkuk skills approve`; plugins stay off until `habakkuk plugins enable`. `habakkuk skills audit` rescans everything and catches files changed after install. See [Plugins](../tools/plugins.md).

## Private files

Everything Habakkuk writes under `$HABAKKUK_HOME` that holds chats, memory, credentials, tokens or keys (databases, `.env`, `auth.json`, logs, agent folders, attachments, browser profiles, backups) is created `0600` in folders `0700`. `habakkuk doctor` and `habakkuk security` warn about anything wider, and `habakkuk security -fix` tightens it. On Windows the audit skips these Unix checks; your profile folder is private to you by default.

## No surprise network calls

Habakkuk sends no usage telemetry and checks for updates only when you run `habakkuk update`. It contacts:

- your model providers and the channels you configured;
- services you named in `config.yaml`: a `search:` provider, `tts`, `images`, `video`, `realtime`, `memory.embeddings`, `telemetry.otlp_endpoint`, MCP servers, A2A peers, Home Assistant;
- the pages an agent reads with `fetch_url` or the browser.

There is no default web search provider: the `web_search` tool exists only once `search:` names one. `habakkuk security -network` lists every endpoint and whether your config turns it on.

## The local HTTP server

The HTTP server listens on loopback unless a token is set, and then refuses requests addressed to other host names (DNS rebinding). Every request a browser sends from another origin (another site, another port on localhost, a sandboxed frame) is refused, including WebSocket handshakes and form posts. CORS is never granted, and the dashboard's changes need JSON bodies, which cross-site pages cannot send without a refused preflight.

So a web page you visit cannot drive the agent, read your chats or pair a device, with or without a token. Programs (curl, SDKs, webhook senders, devices) authenticate with the token, a webhook secret or a device secret.

## The audit

`habakkuk security` (also `habakkuk security audit`) checks:

- file modes under `$HABAKKUK_HOME`;
- agents in `full` mode or reachable by outsiders, and open channels;
- the HTTP listener and its token, and webhook secrets;
- risky MCP servers, and skills and hooks others can write;
- backups and free disk space.

It exits non-zero while a high or critical finding remains, so it can run in a check.

## Secret managers

Settings ending in `_env` name a variable. `secrets:` can resolve those variables from a secret manager instead of `.env`:

```yaml
secrets:                          # resolve *_env variables from secret managers
  sources:
    1password: {}                 # op read
    bitwarden: {}                 # bw get (ITEM, or ITEM#field)
    file: {}
    pass: {type: command, command: 'pass show "$HABAKKUK_SECRET_REF"', cache: 10m}
  refs:
    ANTHROPIC_API_KEY: op://Private/Anthropic/credential
    TELEGRAM_BOT_TOKEN: pass:habakkuk/telegram
```

A variable is read from the process environment first, then a reference, then `.env`. Secret values are redacted from the log and from the dashboard's config page.

## Reporting a vulnerability

See [SECURITY.md](../../SECURITY.md) in the repository.
