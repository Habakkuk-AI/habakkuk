---
title: Signal
description: "Connect a Signal number through a signal-cli daemon: register or link the number, keep the daemon on loopback, and approve people by phone number."
order: 6
---

To put Habakkuk on a Signal number, you run [signal-cli](https://github.com/AsamK/signal-cli) as a daemon in HTTP mode, and Habakkuk talks to it. signal-cli keeps the account's keys, so Habakkuk never sees them.

## Set it up

Give the bot its own number. Then you have two choices. You can register the number:

```bash
signal-cli -a +15551234567 register             # add --captcha ... if Signal asks for one
signal-cli -a +15551234567 verify 123-456       # the code from the SMS
```

Or you can link signal-cli to an existing phone as a secondary device. Run `signal-cli link -n habakkuk`, which prints a `sgnl://` URI. Turn the URI into a QR code and scan it under Settings > Linked devices.

The daemon has to stay running. You can run it under systemd, for example:

```bash
signal-cli -a +15551234567 daemon --http 127.0.0.1:8080
```

> [!WARNING]
> Keep the daemon on loopback. It has no authentication, so anything that can reach the port can send messages as your number.

Now configure the channel:

```yaml
channels:
  signal:
    type: signal
    account: "+15551234567"           # the bot's number, registered or linked in signal-cli
    api_url: http://127.0.0.1:8080    # the default
```

Run `habakkuk doctor` to check that the daemon answers.

## Access

Add people to `allow_from` by phone number, written like `"+15557654321"`. Or approve them with `habakkuk pair`.

## Groups

In a group, the bot answers when you mention it, reply to it, or start your message with the agent's name.

## What works

- Text and attachments work in both directions (up to 100 MB).
- The bot shows a typing indicator.
- You answer approvals with **yes** or **no**.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `signal-cli daemon not reachable at http://127.0.0.1:8080` | Start the daemon with `--http`, or set `api_url` to where it listens. |
| `signal needs account, the bot's number in international format` | Set `account` with the leading `+`. |
| Messages arrive in the phone but not in Habakkuk | A linked signal-cli only receives while the daemon runs. Check it under your service manager. |
