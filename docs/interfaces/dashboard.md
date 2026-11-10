---
title: Dashboard
description: "The http section turns on a control dashboard at /app/: chat, sessions, board, schedules, memory, skills, usage, approvals and config, plus the other endpoints the same server offers."
order: 2
---

An `http` section makes `habakkuk serve` also serve a chat page, a control dashboard, a JSON API and webhooks. `habakkuk setup` adds one on loopback to every new home, so the dashboard is there from the first `habakkuk serve` (`serve` prints its address):

```yaml
http:
  listen: 127.0.0.1:8484            # the default; a public address needs token_env
  token_env: HABAKKUK_HTTP_TOKEN    # Authorization: Bearer <token>
  public_url: https://habakkuk.example.com   # optional: how the internet reaches this server
```

> [!IMPORTANT]
> Without `token_env` the server refuses any address but loopback, and answers only requests whose `Host` is a loopback name, so a web page cannot reach it through DNS rebinding. To reach it from elsewhere, set a token and put it behind HTTPS (a reverse proxy, or `habakkuk expose tailscale`). See [Remote access](../deploy/remote-access.md).

## The dashboard

`http://127.0.0.1:8484/app/` is a control dashboard with these pages: overview, chat, sessions, board, schedules, memory, skills, usage, approvals and config. It is plain HTML, CSS and JavaScript embedded in the binary, over the JSON API under `/api/` with the same token ([HTTP API](../reference/api.md)).

- **Overview** starts with a **Get started** panel on a new install: say hi, turn on a morning brief, teach your agent something. Each one opens Chat with the message ready to send. The panel goes away by itself once the agent has answered a message, or you have a schedule or a second chat channel. Hide removes it sooner.
- **Empty pages explain themselves.** Chat, Sessions, Schedules, Memory, Skills and the Board each say what belongs there when they are empty and offer a one-click message that fills them.
- **Chat** shows tool calls live, sends files as attachments and voice recordings for transcription, reads replies aloud through `tts`, and with `realtime:` offers Live mode (see [Voice](../tools/voice.md#live-mode-in-the-dashboard)).
- **Approvals** waiting in any chat can be answered from the dashboard.
- **Config** shows the configuration with the values of every secret variable, secret-named field and URL credential redacted. For each chat channel it has two settings you can change there: what the chat shows while the agent works (a short status line, every step, or nothing) and what approval requests show (a plain summary, or the exact command). Saving edits `config.yaml` in place and applies from the next message, with no restart.
- A service worker makes it installable as an app.

[Habakkuk Desktop](desktop-app.md) wraps the same dashboard in a native window with notifications.

## Everything the server offers

| Path | What it is | Page |
|---|---|---|
| `/` | The chat page | [Web chat](../channels/web-chat.md) |
| `/app/` | The dashboard | this page |
| `/api/` | The JSON API the dashboard uses | [HTTP API](../reference/api.md) |
| `/healthz` | Channel states and queue depth, without a token | [Observability](../deploy/observability.md) |
| `/hooks/<name>` | Signed webhooks that start a turn | [Integrations: webhooks](../tools/integrations.md#webhooks) |
| `/channels/<name>` | Webhook channels (SMS, Teams, Google Chat and others) | [Webhook channels](../channels/settings.md#webhook-channels) |
| `/v1/` | The OpenAI-compatible API | [Integrations](../tools/integrations.md#openai-compatible-api) |
| `/mcp` | Habakkuk as an MCP server, with `mcp_server.http` | [MCP](../tools/mcp.md#habakkuk-as-an-mcp-server) |
| `/a2a` | Agent2Agent, with `a2a.serve` | [Integrations](../tools/integrations.md#agent2agent) |
| `/nodes` | Device nodes, with a `nodes` section | [Integrations](../tools/integrations.md#device-nodes) |
| `/metrics` | Prometheus metrics | [Observability](../deploy/observability.md#metrics-and-traces) |

## Security

The server is locked down for browsers. Every request a browser sends from another origin (another site, another port on localhost, a sandboxed frame) is refused, including WebSocket handshakes and form posts. CORS is never granted, and the dashboard's changes need JSON bodies. A web page you visit cannot drive the agent, read your chats or pair a device, with or without a token. See [Security](../deploy/security.md#the-local-http-server).
