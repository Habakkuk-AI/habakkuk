---
title: Dashboard
description: "Turn on the control dashboard at /app/ with an http section, see what its pages do, and find the other endpoints the same server offers."
order: 2
---

With an `http` section in your config, `hk serve` also serves a chat page, a control dashboard, a JSON API and webhooks. You probably have one already. `hk setup` adds an `http` section on loopback (an address only this computer can reach) to every new home. So the dashboard is there from your first `hk serve`, and `serve` prints its address. The section looks like this:

```yaml
http:
  listen: 127.0.0.1:8484            # the default; a public address needs token_env
  token_env: HABAKKUK_HTTP_TOKEN    # Authorization: Bearer <token>
  public_url: https://habakkuk.example.com   # optional: how the internet reaches this server
```

> [!IMPORTANT]
> Without `token_env`, the server accepts only a loopback address. It also answers only requests whose `Host` is a loopback name, so a web page can't reach it through DNS rebinding. To reach the server from anywhere else, set a token and put it behind HTTPS. You can do that with a reverse proxy, or with `hk expose tailscale`. See [Remote access](../deploy/remote-access.md).

## The dashboard

Open `http://127.0.0.1:8484/app/` and you get a control dashboard. Its pages are overview, chat, sessions, board, schedules, memory, skills, usage, approvals and config. The dashboard is plain HTML, CSS and JavaScript embedded in the binary. It works over the JSON API under `/api/` and uses the same token (see [HTTP API](../reference/api.md)).

- **Overview** opens with a **Get started** panel on a new install. The panel has three suggestions: say hi, turn on a morning brief, and teach your agent something. Each one opens Chat with the message ready to send. The panel goes away by itself once the agent has answered a message, or once you have a schedule or a second chat channel. To remove it sooner, use Hide.
- **Empty pages explain themselves.** When Chat, Sessions, Schedules, Memory, Skills or the Board is empty, the page says what belongs there. It also offers a message you can send with one click to fill it.
- **Chat** shows tool calls as they happen. It sends files as attachments and voice recordings for transcription, and it reads replies aloud through `tts`. With `realtime:` it also offers Live mode (see [Voice](../tools/voice.md#live-mode-in-the-dashboard)).
- **Approvals** that are waiting in any chat can be answered from the dashboard.
- **Config** shows your configuration. The values of every secret variable, secret-named field and URL credential are redacted. For each chat channel, there are two settings you can change on this page. The first is what the chat shows while the agent works: a short status line, every step, or nothing. The second is what approval requests show: a plain summary, or the exact command. When you save, the dashboard edits `config.yaml` in place. The change applies from the next message, and you don't need to restart.
- A service worker lets you install the dashboard as an app.

Want the dashboard in a window of its own? [Habakkuk Desktop](desktop-app.md) wraps the same dashboard in a native window and adds notifications.

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

The server is locked down for browsers. It refuses every request a browser sends from another origin. Another origin means another site, another port on localhost, or a sandboxed frame. The rule covers WebSocket handshakes and form posts too. The server never grants CORS, and any change the dashboard makes needs a JSON body. So a web page you visit can't drive the agent, read your chats or pair a device, with or without a token. See [Security](../deploy/security.md#the-local-http-server).
