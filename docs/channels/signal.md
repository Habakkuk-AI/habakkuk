---
title: Signal
description: "Connect a Signal number through a signal-cli daemon: register or link the number, keep the daemon on loopback, and approve people by phone number."
order: 6
---

Habakkuk talks to [signal-cli](https://github.com/AsamK/signal-cli) running as a daemon in HTTP mode. signal-cli keeps the account's keys; Habakkuk never sees them.

## Set it up

Give the bot its own number. Then either register it:

```bash
signal-cli -a +15551234567 register             # add --captcha ... if Signal asks for one
signal-cli -a +15551234567 verify 123-456       # the code from the SMS
```

or link signal-cli to an existing phone as a secondary device: `signal-cli link -n habakkuk` prints a `sgnl://` URI. Turn it into a QR code and scan it under Settings > Linked devices.

Keep the daemon running, for example under systemd:

```bash
signal-cli -a +15551234567 daemon --http 127.0.0.1:8080
```

> [!WARNING]
> Keep the daemon on loopback. It has no authentication: anything that can reach the port can send as your number.

Then configure the channel:

```yaml
channels:
  signal:
    type: signal
    account: "+15551234567"           # the bot's number, registered or linked in signal-cli
    api_url: http://127.0.0.1:8080    # the default
```

`habakkuk doctor` checks that the daemon answers.

## Access

Add people to `allow_from` by number (`"+15557654321"`), or approve them with `habakkuk pair`.

## Groups

In groups the bot answers mentions, replies, and messages that start with the agent's name.

## What works

- Text and attachments in and out (up to 100 MB).
- Typing indicator.
- Approvals are answered with **yes** or **no**.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `signal-cli daemon not reachable at http://127.0.0.1:8080` | Start the daemon with `--http`, or set `api_url` to where it listens. |
| `signal needs account, the bot's number in international format` | Set `account` with the leading `+`. |
| Messages arrive in the phone but not in Habakkuk | A linked signal-cli only receives while the daemon runs. Check it under your service manager. |
