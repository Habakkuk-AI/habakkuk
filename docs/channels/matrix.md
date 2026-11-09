---
title: Matrix
description: "Connect a Matrix account with an access token: invites from allowed users, direct chats, rooms and threads; end-to-end encryption is not supported yet."
order: 7
---

Habakkuk reads Matrix with `/sync` long polling, so it needs no public URL and works with any homeserver.

## Set it up

1. Make an account for the bot on your homeserver.
2. Get an access token, for example:

```bash
curl -s -X POST https://matrix.example.org/_matrix/client/v3/login -H 'Content-Type: application/json' \
  -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"habakkuk"},"password":"...","initial_device_display_name":"habakkuk"}'
```

3. Put the `access_token` in `MATRIX_ACCESS_TOKEN` in `.env`, and configure the channel:

```yaml
channels:
  matrix:
    type: matrix
    homeserver: https://matrix.example.org
    token_env: MATRIX_ACCESS_TOKEN
    account: "@habakkuk:example.org"  # optional; checked against the token
    allow_from: ["@you:example.org"]  # also who may invite the bot into rooms
```

4. Invite the bot from an account in `allow_from` (or one approved with `habakkuk pair`). It joins on its own and ignores invitations from anyone else.

> [!IMPORTANT]
> End-to-end encryption is not supported yet. Create the room without encryption. In an encrypted room the bot says once that it cannot read it, and stays quiet after that.

## Access

`allow_from` takes Matrix user ids (`@you:example.org`). The same list decides whose invitations the bot accepts. A room with only you and the bot is a direct chat.

## What works

- Text and files in; text and files up to 50 MB out.
- Mentions and replies in rooms, and threads.
- Typing indicator.
- The sync position is kept in `channels/matrix-<name>.json`, so a restart picks up where it stopped.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `matrix: the access token belongs to @x:example.org, not @y:example.org` | `account` does not match the token. Fix `account`, or remove it. |
| `matrix needs homeserver (an http:// or https:// URL) and token_env` | Both settings are required. |
| The bot does not join a room | The invitation came from someone not in `allow_from`. |
| The bot says it cannot read the room | The room is encrypted. Make a new room without encryption. |
