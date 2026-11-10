---
title: Microsoft Teams
nav_title: Teams
description: "Connect a Teams bot through the Bot Framework: the bot registration, the messaging endpoint, single-tenant bots, and how files are sent."
order: 10
---

On Microsoft Teams, Habakkuk is a bot that works through the Microsoft Bot Framework. The channel type is `msteams`. Teams posts activities to Habakkuk's HTTP server, each with a bearer token signed by the Bot Framework. Habakkuk checks that token against the Bot Framework's published keys. Replies go to the conversation's service URL, with an app token from client credentials.

> [!NOTE]
> Teams is a [webhook channel](settings.md#webhook-channels). You need an `http` section with a `public_url` that the Bot Framework can reach.

## Set it up

1. Register a bot, either as an Azure Bot resource or in the Teams Developer Portal. Note its **app id**, then create a **client secret**.
2. Set the bot's messaging endpoint to `<public_url>/channels/<name>`. For example, `https://habakkuk.example.com/channels/teams`.
3. Turn on the Microsoft Teams channel for the bot. Then install the app in Teams for the people or teams who'll use it.
4. Put the secret in `.env`, then configure the channel:

```yaml
channels:
  teams:
    type: msteams
    msteams:
      app_id: 00000000-0000-0000-0000-000000000000
      app_password_env: TEAMS_APP_SECRET
      # tenant_id: 11111111-1111-1111-1111-111111111111   # single-tenant bots only
http:
  listen: 127.0.0.1:8484
  public_url: https://habakkuk.example.com
```

Set `tenant_id` only for a single-tenant bot. Without it, the token comes from the multi-tenant Bot Framework authority.

## Access

`allow_from` takes the user's Microsoft Entra object id. The Bot Framework user id matches too. Anyone else who writes to the bot in a one-to-one chat gets a pairing code. Send `/whoami` to see both ids.

## Channels and threads

In team channels and group chats, the bot answers when you mention it, and it replies in the thread.

## What works

- The bot receives text, files and pasted images.
- It sends text. In a one-to-one chat, a file goes out as a consent card that the user accepts. In channels, images are sent inline.
- It shows a typing indicator.
- You answer approvals with **yes** or **no**.

Habakkuk learns each conversation's service URL from the messages it receives, and keeps it in `channels/msteams-<name>.json`. That's why schedules and `habakkuk send` can reach a Teams conversation only after someone has written in it.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Messages never arrive | Check the messaging endpoint, that `public_url` reaches `listen`, and `habakkuk doctor`'s webhook URL. |
| Replies fail with an auth error | Check `app_id` and the secret. For a single-tenant bot, set `tenant_id`. |
| A scheduled message to Teams fails | Nobody has written in that conversation since the channel was set up. Send the bot a message there first. |
