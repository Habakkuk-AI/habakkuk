---
title: SMS
description: "Text Habakkuk over SMS and MMS through a Twilio number: the webhook, signature checks, messaging services and media limits."
order: 9
---

The `sms` channel uses Twilio. Twilio posts inbound SMS and MMS to Habakkuk's HTTP server, signed with the account's auth token; replies go out through Twilio's Messages API.

> [!NOTE]
> SMS is a [webhook channel](settings.md#webhook-channels): Twilio must reach `habakkuk serve` over the internet, so you need an `http` section with a `public_url`.

## Set it up

1. In Twilio, buy or pick a number that can send and receive SMS (and MMS, for media).
2. Set the number's **A message comes in** webhook (HTTP POST) to `<public_url>/channels/<name>`, for example `https://habakkuk.example.com/channels/sms`. `habakkuk doctor` prints the URL.
3. Put the auth token in `.env` and configure the channel:

```yaml
channels:
  sms:
    type: sms
    allow_from: ["15557654321"]            # numbers without +
    sms:
      account_sid: ACxxxxxxxx
      auth_token_env: TWILIO_AUTH_TOKEN
      from: "+15551234567"                 # or messaging_service_sid: MGxxxxxxxx
http:
  listen: 127.0.0.1:8484
  public_url: https://habakkuk.example.com
```

Use `messaging_service_sid` instead of `from` if you send through a Twilio messaging service.

> [!IMPORTANT]
> Twilio signs the exact URL it calls. `public_url` must match the webhook URL in Twilio character for character, or every request is refused.

## Access

`allow_from` takes phone numbers without the `+`. Anyone else who texts gets a pairing code.

## What works

- SMS and MMS media in; SMS and MMS out, with media up to 5 MB. Twilio fetches outgoing media from a one-hour URL on Habakkuk's server.
- No typing indicator or buttons: approvals are answered by texting **yes** or **no**.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Texts never arrive | Check the webhook URL in Twilio, that `public_url` matches it exactly, and that the proxy or tunnel reaches `listen`. |
| Media is not delivered | The file is over 5 MB, or Twilio cannot reach `public_url` to fetch it. |
