---
title: Matrix
description: "Connect a Matrix account with an access token: invites from allowed users, direct chats, rooms and threads; end-to-end encryption is not supported yet."
order: 7
---

Habakkuk reads Matrix with `/sync` long polling, so you don't need a public URL and it works with any homeserver.

## Set it up

1. Create an account for the bot on your homeserver.
2. Get an access token for it. Here's one way:

```bash
curl -s -X POST https://matrix.example.org/_matrix/client/v3/login -H 'Content-Type: application/json' \
  -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"habakkuk"},"password":"...","initial_device_display_name":"habakkuk"}'
```

3. Put the `access_token` in `.env` as `MATRIX_ACCESS_TOKEN`. Then configure the channel:

```yaml
channels:
  matrix:
    type: matrix
    homeserver: https://matrix.example.org
    token_env: MATRIX_ACCESS_TOKEN
    account: "@habakkuk:example.org"  # optional; checked against the token
    allow_from: ["@you:example.org"]  # also who may invite the bot into rooms
```

4. Invite the bot from an account that's in `allow_from`, or one you've approved with `habakkuk pair`. The bot joins on its own. It ignores invitations from anyone else.

> [!IMPORTANT]
> End-to-end encryption isn't supported yet, so create the room without encryption. In an encrypted room, the bot says once that it can't read the room. After that it stays quiet.

## Access

`allow_from` takes Matrix user ids, such as `@you:example.org`. The bot also uses this list to decide whose invitations it accepts. A room with only you and the bot in it counts as a direct chat.

## What works

- The bot receives text and files. It sends text, and files up to 50 MB.
- In rooms, mentions and replies work. So do threads.
- It shows a typing indicator.
- Habakkuk keeps the sync position in `channels/matrix-<name>.json`, so after a restart it picks up where it stopped.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `matrix: the access token belongs to @x:example.org, not @y:example.org` | `account` does not match the token. Fix `account`, or remove it. |
| `matrix needs homeserver (an http:// or https:// URL) and token_env` | Both settings are required. |
| The bot does not join a room | The invitation came from someone not in `allow_from`. |
| The bot says it cannot read the room | The room is encrypted. Make a new room without encryption. |
