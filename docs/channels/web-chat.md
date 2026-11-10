---
title: Web chat
description: "The chat page Habakkuk serves itself, and the same chat over a JSON API with Server-Sent Events, for your own clients."
order: 13
---

Web chat is the chat page Habakkuk serves itself. Use it when you want to talk to an agent from a browser, or from a client you write. When your config has an `http` section, `hk serve` serves a small chat page at `/`, and the same chat over a JSON API. You don't need an account anywhere else. The [dashboard](../interfaces/dashboard.md) and [desktop app](../interfaces/desktop-app.md) use it too.

## Set it up

Run `hk setup` and pick **web chat on this computer**. Or add an `http` section by hand:

```yaml
http:
  listen: 127.0.0.1:8484            # the default
  token_env: HABAKKUK_HTTP_TOKEN    # Authorization: Bearer <token>; required off loopback
```

The `http` section adds a channel of type `web`, named `web`, on its own. You only need to configure one yourself when you want to change its settings:

```yaml
channels:
  web:
    type: web
    agent: habakkuk
```

Then open `http://127.0.0.1:8484/`. The page works on a phone and follows the system's dark mode. It keeps your conversation list in the browser, and it asks for the token once.

## Access

The bearer token decides who gets in. Anyone who holds it may talk, so a `web` channel allows every sender by default. Without `token_env`, the server refuses any address except loopback. To reach it from elsewhere, set a token and put the server behind HTTPS. See [Remote access](../deploy/remote-access.md).

Web chat sessions never go idle. A conversation keeps its session until you send `/new`.

## The chat API

```bash
curl -s localhost:8484/healthz                                  # no token: channel states and queue depth
curl -s -H "Authorization: Bearer $HABAKKUK_HTTP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"conversation":"desk","text":"what is on today?"}' localhost:8484/api/chat
curl -sN -H "Authorization: Bearer $HABAKKUK_HTTP_TOKEN" 'localhost:8484/api/chat/desk/events?after=0'   # Server-Sent Events
```

- A conversation id is any name you pick. It can use letters, digits, `.`, `_`, `:` and `-`.
- To talk to another agent, add `"agent": "ops"`. To make a retry safe, add `"id"`.
- The event stream replays everything after `after` (or `Last-Event-ID`), then follows live. A client that drops can reconnect with the last id it saw.
- Commands such as `/new` work the way they do in any other channel, and so do chat approvals.

You'll find every endpoint in the [HTTP API reference](../reference/api.md#chat). If you're using an OpenAI client, use the [OpenAI-compatible API](../tools/integrations.md#openai-compatible-api) instead.

## What works

- Text works in both directions, with the typing indicator.
- The dashboard's chat adds more: tool calls shown live, file attachments, voice recordings, replies read aloud and Live voice mode.
- You can use the dashboard to answer an approval that's waiting in any chat.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `a channel of type web needs the http section` | Add an `http` section. |
| `http: listen 0.0.0.0:8484 is not a loopback address; set token_env so the API needs a token` | Set `token_env`, or listen on `127.0.0.1`. |
| A request from another web page is refused | By design: requests a browser sends from another origin are refused, so a site you visit cannot drive the agent. Use the chat page itself, or call the API from a program with the token. |
