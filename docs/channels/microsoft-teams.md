---
title: Microsoft Teams
nav_title: Teams
description: "Connect a Teams bot through the Bot Framework: the bot registration, the messaging endpoint, single-tenant bots, and how files are sent."
order: 10
---

The `msteams` channel is a bot through the Microsoft Bot Framework. Teams posts activities to Habakkuk's HTTP server with a bearer token signed by the Bot Framework, which Habakkuk checks against its published keys. Replies go to the conversation's service URL with an app token from client credentials.

> [!NOTE]
> Teams is a [webhook channel](settings.md#webhook-channels): it needs an `http` section with a `public_url` that the Bot Framework can reach.

## Set it up

1. Register a bot (an Azure Bot resource, or the Teams Developer Portal). Note its **app id** and create a **client secret**.
2. Set the bot's messaging endpoint to `<public_url>/channels/<name>`, for example `https://habakkuk.example.com/channels/teams`.
3. Turn on the Microsoft Teams channel for the bot, and install the app in Teams for the people or teams that should use it.
4. Put the secret in `.env` and configure the channel:

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

`tenant_id` is for single-tenant bots; without it the token comes from the multi-tenant Bot Framework authority.

## Access

`allow_from` takes the user's Microsoft Entra object id (the Bot Framework user id also matches). Anyone else who writes to the bot in a one-to-one chat gets a pairing code. `/whoami` shows both ids.

## Channels and threads

In team channels and group chats the bot answers when mentioned, and replies in the thread.

## What works

- Text, files and pasted images in.
- Text out. Files go out in one-to-one chats as a consent card the user accepts; in channels, images are sent inline.
- Typing indicator.
- Approvals are answered with **yes** or **no**.

Habakkuk learns each conversation's service URL from the messages it receives and keeps it in `channels/msteams-<name>.json`. So schedules and `habakkuk send` can reach a Teams conversation only after someone has written in it.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Messages never arrive | Check the messaging endpoint, that `public_url` reaches `listen`, and `habakkuk doctor`'s webhook URL. |
| Replies fail with an auth error | Check `app_id` and the secret. For a single-tenant bot, set `tenant_id`. |
| A scheduled message to Teams fails | Nobody has written in that conversation since the channel was set up. Send the bot a message there first. |
