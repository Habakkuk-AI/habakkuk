---
title: Web chat
description: "The chat page Habakkuk serves itself, and the same chat over a JSON API with Server-Sent Events, for your own clients."
order: 13
---

With an `http` section, `habakkuk serve` serves a small chat page at `/` and the same chat over a JSON API. It needs no outside account, and the [dashboard](../interfaces/dashboard.md) and [desktop app](../interfaces/desktop-app.md) use it too.

## Set it up

Run `habakkuk setup` and pick **web chat on this computer**, or add an `http` section by hand:

```yaml
http:
  listen: 127.0.0.1:8484            # the default
  token_env: HABAKKUK_HTTP_TOKEN    # Authorization: Bearer <token>; required off loopback
```

The `http` section adds a channel of type `web` named `web` by itself. Configure one yourself only to change its settings:

```yaml
channels:
  web:
    type: web
    agent: habakkuk
```

Then open `http://127.0.0.1:8484/`. The page works on a phone, follows the system's dark mode, keeps your conversation list in the browser and asks for the token once.

## Access

The bearer token is the gate: everyone who holds it may talk, so a `web` channel allows every sender by default. Without `token_env` the server refuses any address but loopback. To reach it from elsewhere, set a token and put it behind HTTPS; see [Remote access](../deploy/remote-access.md).

Web chat sessions never go idle: a conversation keeps its session until you send `/new`.

## The chat API

```bash
curl -s localhost:8484/healthz                                  # no token: channel states and queue depth
curl -s -H "Authorization: Bearer $HABAKKUK_HTTP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"conversation":"desk","text":"what is on today?"}' localhost:8484/api/chat
curl -sN -H "Authorization: Bearer $HABAKKUK_HTTP_TOKEN" 'localhost:8484/api/chat/desk/events?after=0'   # Server-Sent Events
```

- A conversation id is any name you pick (letters, digits, `.`, `_`, `:`, `-`).
- Add `"agent": "ops"` to talk to another agent, and `"id"` to make a retry safe.
- The event stream replays everything after `after` (or `Last-Event-ID`), then follows live, so a client that drops simply reconnects with the last id it saw.
- Commands such as `/new` and chat approvals work as in any other channel.

The [HTTP API reference](../reference/api.md#chat) has every endpoint. For OpenAI clients, use the [OpenAI-compatible API](../tools/integrations.md#openai-compatible-api) instead.

## What works

- Text in and out, with the typing indicator.
- The dashboard's chat adds tool calls shown live, file attachments, voice recordings, replies read aloud and Live voice mode.
- Approvals waiting in any chat can be answered from the dashboard.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `a channel of type web needs the http section` | Add an `http` section. |
| `http: listen 0.0.0.0:8484 is not a loopback address; set token_env so the API needs a token` | Set `token_env`, or listen on `127.0.0.1`. |
| A request from another web page is refused | By design: requests a browser sends from another origin are refused, so a site you visit cannot drive the agent. Use the chat page itself, or call the API from a program with the token. |
