---
title: iMessage
description: "Reach Habakkuk over iMessage through a BlueBubbles server on a Mac: the webhook secret, the Private API for typing and replies, and group chats."
order: 12
---

Habakkuk reaches iMessage through [BlueBubbles](https://bluebubbles.app), a server that runs on a Mac signed in to iMessage. BlueBubbles posts each new message to Habakkuk's HTTP server, at a webhook URL that carries a secret. Replies, files and typing go back through the BlueBubbles REST API. The channel type is `bluebubbles`.

> [!NOTE]
> iMessage is a [webhook channel](settings.md#webhook-channels), so the BlueBubbles server has to reach Habakkuk's HTTP server. If both run on your own network, a `public_url` on that network is enough.

## Set it up

1. Install BlueBubbles Server on a Mac that stays on. Sign in to iMessage there and set a server password.
2. Pick a webhook secret. Put the secret and the server password in `.env`.
3. In BlueBubbles, add a webhook for new messages. Point it at `<public_url>/channels/<name>?secret=<webhook secret>`.
4. Configure the channel:

```yaml
channels:
  imessage:
    type: bluebubbles
    bluebubbles:
      url: http://mac.local:1234             # the BlueBubbles server
      password_env: BLUEBUBBLES_PASSWORD
      webhook_secret_env: BLUEBUBBLES_WEBHOOK_SECRET   # must match ?secret= on the webhook URL
      private_api: true                      # the server has the Private API: typing, replies
      mention: Habakkuk                      # the default
http:
  listen: 127.0.0.1:8484
  public_url: https://habakkuk.example.com
```

## Access

`allow_from` takes the sender's phone number without the `+`, or their email address. Everyone else gets a pairing code.

## Group chats

In a group chat, Habakkuk treats a message as addressed to it when the message names it. That means the message contains the `mention` text, which is `Habakkuk` by default.

## What works

- Text and attachments work in both directions.
- With `private_api: true`, you also get a typing indicator and threaded replies. This needs the BlueBubbles Private API set up on the Mac.
- You answer approvals with **yes** or **no**.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Messages never arrive | Check the webhook URL in BlueBubbles includes `?secret=` with the same value as `webhook_secret_env`. |
| Replies fail | Check `url` and the server password, and that the Mac is awake. |
| No typing indicator | Turn on the Private API in BlueBubbles, then set `private_api: true`. |
