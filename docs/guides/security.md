---
title: Security model
description: "What Habakkuk lets an agent do by default: approvals for chat-driven and tainted turns, guests, vetted skills, private files, no surprise network calls, and a locked-down local server."
order: 8
---

Habakkuk is safe by default, and `habakkuk security` shows what it allows on your machine: the effective tool policy of each agent, then an audit with fixes. `habakkuk status` shows the policy too.

**Risky capabilities.** Five kinds of tool call can hurt the machine Habakkuk runs on: `exec` (the shell, and MCP servers marked `risk: exec`), `process` (input to and killing background processes), `write` (files outside the agent's workspace), `computer` (screen, mouse, keyboard, and a paired device's screen, camera and clipboard) and `browser` (pages on loopback or private addresses that `browser.allow_private` lets through).

**Chat channels are least-privilege.** A turn that comes from a messaging channel (anything but the CLI, the TUI and the local dashboard) needs approval for every risky capability, whatever the permission mode, `full` included. You approve in the chat. To let your own chat messages act like the CLI, opt in per agent:

```yaml
agents:
  habakkuk:
    trust: full               # or lift only some: channel_allow: [write, browser]
```

**Strangers never get them.** A sender let in by `allow_from: ["*"]` (rather than by id or `habakkuk pair`) is a guest: guests' turns are refused every risky capability, with or without approval, cannot answer your pending approvals, and cannot use `/remember`, `/forget` or `/agent`. A turn with no sender behind it (a schedule) on an account open to everyone counts as a guest's.

**Outside content taints a turn.** Once a turn has read outside content (a web page, a search result, an MCP or other network tool, an email, a webhook or Home Assistant payload, a device's output), `exec`, `process` and writes outside the workspace need a human's approval, even in `full` mode and even with `approvals: reviewer` (a model is not asked: it could be steered by the same text). With nobody attending, such calls are denied. A delegated task inherits its parent's taint and origin. `allow_tainted_exec: true` on an agent turns this off; the audit flags it.

**Skills and plugins from outside** are pinned and checked. An install prints the source, the commit, the tree hash, every file and the scripts among them, and scans them; a high-risk finding (`curl | sh`, decoded payloads run, credentials or the environment sent out, obfuscation, program binaries) refuses the install without `-force`. The lock file records the commit and hash. Updates show a diff and apply only when you say yes. A skill that ships scripts stays disabled until `habakkuk skills approve`; plugins stay off until `habakkuk plugins enable`. `habakkuk skills audit` rescans everything and catches files changed after install. See [Plugins](plugins.md).

**Private files.** Everything Habakkuk writes under `$HABAKKUK_HOME` that holds chats, memory, credentials, tokens or keys (databases, `.env`, `auth.json`, logs, agent folders, attachments, browser profiles, backups) is created `0600` in folders `0700`. `habakkuk doctor` and `habakkuk security` warn about anything wider, and `habakkuk security -fix` tightens it.

**No surprise network calls.** Habakkuk sends no usage telemetry and checks for updates only when you run `habakkuk update`. It contacts your model providers, the channels you configured, and services you named in config.yaml (a `search:` provider, `tts`, `images`, `video`, `realtime`, `memory.embeddings`, `telemetry.otlp_endpoint`, MCP servers, A2A peers, Home Assistant), and the pages an agent reads with `fetch_url` or the browser. There is no default web search provider: `web_search` exists only once `search:` names one. `habakkuk security -network` lists every endpoint and whether your config turns it on.

**The local HTTP server** listens on loopback unless a token is set, and then refuses requests addressed to other host names (DNS rebinding). Every request a browser sends from another origin (another site, another port on localhost, a sandboxed frame) is refused, including WebSocket handshakes and form posts; CORS is never granted; the dashboard's changes need JSON bodies (which cross-site pages cannot send without a refused preflight). So a web page you visit cannot drive the agent, read your chats or pair a device, with or without a token. Programs (curl, SDKs, webhook senders, devices) authenticate with the token, a webhook secret or a device secret.

One exception: a `whatsappagent` channel only ever hears from the person who created the agent (WhatsApp refuses everyone else), so its sender is always treated as the owner, even with `allow_from: "*"`.
