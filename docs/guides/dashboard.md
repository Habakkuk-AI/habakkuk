---
title: Dashboard and web chat
description: "The http section: the chat page, the control dashboard at /app/, the chat API with Server-Sent Events, and signed webhooks."
order: 15
---

An `http` section makes `habakkuk serve` also serve a chat page, a control dashboard, a JSON API and webhooks:

```yaml
http:
  listen: 127.0.0.1:8484            # the default; a public address needs token_env
  token_env: HABAKKUK_HTTP_TOKEN    # Authorization: Bearer <token>
  webhooks:
    alerts:                         # POST /hooks/alerts
      agent: habakkuk
      secret_env: ALERTS_SECRET     # HMAC-SHA256 of the body, or a bearer secret
      prompt: "Alert {{.title}} fired. Look into it:\n{{.Body}}"
      deliver: telegram:123456789   # optional; default the web conversation hook-alerts
```

Without `token_env` the server refuses any address but loopback, and answers only requests whose `Host` is a loopback name, so a web page cannot reach it through DNS rebinding. To reach it from elsewhere, set a token and put it behind HTTPS (a reverse proxy, or `habakkuk expose tailscale`; see [Deployment](deployment.md#remote-access)).

## The dashboard

`http://127.0.0.1:8484/app/` is a control dashboard: overview, chat, sessions, board, schedules, memory, skills, usage, approvals and config. It is plain HTML, CSS and JavaScript embedded in the binary, over the JSON API under `/api/` with the same token ([HTTP API](../reference/api.md)).

- **Chat** shows tool calls live, sends files as attachments and voice recordings for transcription, reads replies aloud through `tts`, and with `realtime:` offers Live mode (see [Voice](voice.md#live-mode-in-the-dashboard)).
- **Approvals** waiting in any chat can be answered from the dashboard.
- **Config** shows the configuration with the values of every secret variable, secret-named field and URL credential redacted.
- A service worker makes it installable as an app.

[Habakkuk Desktop](desktop-app.md) wraps the same dashboard in a native window with notifications.

## The chat page

Open `http://127.0.0.1:8484/` for the chat page: it works on a phone, follows the system's dark mode, keeps your conversation list in the browser and asks for the token once. The same chat over the API:

```bash
curl -s localhost:8484/healthz                                  # no token: channel states and queue depth
curl -s -H "Authorization: Bearer $HABAKKUK_HTTP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"conversation":"desk","text":"what is on today?"}' localhost:8484/api/chat
curl -sN -H "Authorization: Bearer $HABAKKUK_HTTP_TOKEN" 'localhost:8484/api/chat/desk/events?after=0'   # Server-Sent Events
```

A conversation id is any name you pick (letters, digits, `.`, `_`, `:`, `-`); add `"agent": "ops"` to talk to another agent, and `"id"` to make a retry safe. The event stream replays everything after `after` (or `Last-Event-ID`), then follows live, so a client that drops simply reconnects with the last id it saw. Commands such as `/new` and chat approvals work as in any other channel.

## Webhooks

A webhook sender signs the raw body with the secret and may name the delivery so retries run once:

```bash
body='{"title":"disk full"}'
sig=$(printf %s "$body" | openssl dgst -sha256 -hmac "$ALERTS_SECRET" -r | cut -d' ' -f1)
curl -s -H "X-Habakkuk-Signature: sha256=$sig" -H 'X-Habakkuk-Delivery: evt-42' -d "$body" localhost:8484/hooks/alerts
```

Each delivery is a fresh, unattended session whose prompt is the hook's template over `{{.Body}}` and the body's top-level JSON fields, framed as outside data rather than instructions. The server answers 202 once the turn is queued; the reply goes to `deliver` (`ACCOUNT:CONVERSATION`) or to the web conversation `hook-<name>`.

## Other endpoints

The same server can also offer an OpenAI-compatible API under `/v1/`, MCP at `/mcp`, Agent2Agent at `/a2a`, device nodes at `/nodes`, Prometheus metrics at `/metrics` and webhook channels at `/channels/<name>`. See [Integrations](integrations.md), [MCP](mcp.md#habakkuk-as-an-mcp-server), [Deployment](deployment.md#metrics-traces-and-logs) and [Channels](channels.md#webhook-channels).
