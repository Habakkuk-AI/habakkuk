---
title: SMS
description: "Text Habakkuk over SMS and MMS through a Twilio number: the webhook, signature checks, messaging services and media limits."
order: 9
---

You can text Habakkuk over SMS and MMS. The `sms` channel uses Twilio. Twilio posts each inbound SMS and MMS to Habakkuk's HTTP server, signed with the account's auth token. Replies go out through Twilio's Messages API.

> [!NOTE]
> SMS is a [webhook channel](settings.md#webhook-channels). Twilio has to reach `habakkuk serve` over the internet, so you need an `http` section with a `public_url`.

## Set it up

1. In Twilio, buy or pick a number that can send and receive SMS. If you want media, it needs MMS too.
2. Set the number's **A message comes in** webhook to `<public_url>/channels/<name>`, with HTTP POST as the method. For example, `https://habakkuk.example.com/channels/sms`. Run `habakkuk doctor` to print the URL.
3. Put the auth token in `.env`, then configure the channel:

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

If you send through a Twilio messaging service, set `messaging_service_sid` in place of `from`.

> [!IMPORTANT]
> Twilio signs the exact URL it calls. Your `public_url` has to match the webhook URL in Twilio character for character. If it doesn't, every request is refused.

## Access

`allow_from` takes phone numbers without the `+`. Anyone else who texts the number gets a pairing code.

## What works

- The channel receives SMS and MMS media. It sends SMS and MMS, with media up to 5 MB. Twilio fetches outgoing media from a URL on Habakkuk's server that lasts one hour.
- There's no typing indicator and there are no buttons. To answer an approval, text **yes** or **no**.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Texts never arrive | Check the webhook URL in Twilio, that `public_url` matches it exactly, and that the proxy or tunnel reaches `listen`. |
| Media is not delivered | The file is over 5 MB, or Twilio cannot reach `public_url` to fetch it. |
