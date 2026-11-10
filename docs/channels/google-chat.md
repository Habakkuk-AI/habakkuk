---
title: Google Chat
description: "Connect a Google Chat app with an HTTP endpoint: the Chat API configuration, the authentication audience, and the service account used for replies."
order: 11
---

On Google Chat, Habakkuk is a Chat app with an HTTP endpoint. The channel type is `googlechat`. Chat posts each event to Habakkuk's HTTP server, with a bearer token that Google signs for the app's audience. Replies go out through the Chat API, authorized as the app's service account.

> [!NOTE]
> Google Chat is a [webhook channel](settings.md#webhook-channels). You need an `http` section with a `public_url` that Google can reach.

## Set it up

1. Open a Google Cloud project and enable the **Google Chat API**.
2. Create a service account, then create a JSON key for it. Habakkuk sends replies as this account.
3. In the Chat API configuration, set the app's connection to an **HTTP endpoint URL**. The URL is `<public_url>/channels/<name>`, for example `https://habakkuk.example.com/channels/gchat`.
4. Note the **Authentication Audience** you chose there. It's either the project number or the endpoint URL.
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

A Chat app built as a Google Workspace add-on works too.

## Access

`allow_from` takes the sender's email address or their Chat user name, which looks like `users/123456789`. Anyone else who sends the bot a direct message gets a pairing code.

## Spaces and threads

In a space, the bot answers when you mention it, and it replies in the thread.

## What works

- The bot receives text and uploads.
- It sends text, in the thread.
- There's no typing indicator and there are no buttons, so you answer approvals with **yes** or **no**.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Every event is refused | `audience` does not match the Authentication Audience in the Chat API configuration. |
| Events arrive but replies fail | Check `credentials_env` holds the service account's JSON key (or a path to it), and that the Chat API is enabled. |
