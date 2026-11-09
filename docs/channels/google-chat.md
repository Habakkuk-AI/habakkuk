---
title: Google Chat
description: "Connect a Google Chat app with an HTTP endpoint: the Chat API configuration, the authentication audience, and the service account used for replies."
order: 11
---

The `googlechat` channel is a Chat app with an HTTP endpoint. Chat posts events to Habakkuk's HTTP server with a bearer token Google signs for the app's audience; replies go through the Chat API, authorized as the app's service account.

> [!NOTE]
> Google Chat is a [webhook channel](settings.md#webhook-channels): it needs an `http` section with a `public_url` that Google can reach.

## Set it up

1. In a Google Cloud project, enable the **Google Chat API**.
2. Create a service account and a JSON key for it. Replies are sent as this account.
3. In the Chat API configuration, set the app's connection to an **HTTP endpoint URL** of `<public_url>/channels/<name>`, for example `https://habakkuk.example.com/channels/gchat`.
4. Note the **Authentication Audience** you chose there: the project number, or the endpoint URL.
5. Configure the channel:

```yaml
channels:
  gchat:
    type: googlechat
    googlechat:
      audience: "123456789012"                   # the project number or endpoint URL set as the Authentication Audience
      credentials_env: GOOGLE_CHAT_CREDENTIALS   # the service account's JSON key, or a path to it
http:
  listen: 127.0.0.1:8484
  public_url: https://habakkuk.example.com
```

Chat apps built as Google Workspace add-ons work too.

## Access

`allow_from` takes the sender's email address or their Chat user name (`users/123456789`). Anyone else who sends a direct message gets a pairing code.

## Spaces and threads

In spaces the bot answers when mentioned, and replies in the thread.

## What works

- Text and uploads in.
- Text out, in the thread.
- No typing indicator or buttons: approvals are answered with **yes** or **no**.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Every event is refused | `audience` does not match the Authentication Audience in the Chat API configuration. |
| Events arrive but replies fail | Check `credentials_env` holds the service account's JSON key (or a path to it), and that the Chat API is enabled. |
