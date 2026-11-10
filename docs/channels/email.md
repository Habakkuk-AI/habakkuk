---
title: Email
description: "Give Habakkuk a mailbox: IMAP with IDLE or polling, SMTP replies that keep the thread, sender checks, and why email sessions start tainted."
order: 8
---

Give Habakkuk a mailbox of its own and you can email it. It reads mail over IMAP, using IDLE when the server has it and polling when it doesn't. It answers over SMTP. Each email thread is one conversation, and replies stay in that thread. You don't need a public URL.

## Set it up

1. Create a mailbox for the agent. If your provider uses app passwords, create one of those too.
2. Put the password in `.env`, then configure the channel:

```yaml
channels:
  mail:
    type: email
    allow_from: [you@example.com]
    email:
      address: habakkuk@example.com
      imap: imap.example.com:993             # IDLE when the server has it, else polling every minute
      smtp: smtp.example.com:587             # STARTTLS; 465 means implicit TLS
      password_env: HABAKKUK_MAIL_PASSWORD
```

### All settings

| Setting | Meaning |
|---|---|
| `address` | Habakkuk's address; replies come from it |
| `name` | The display name on replies (default Habakkuk) |
| `imap`, `smtp` | `host:port` of each server |
| `imap_tls`, `smtp_tls` | `auto` (default: TLS on 993 and 465, else STARTTLS), `tls`, `starttls` or `none` |
| `username` | The login (default the address) |
| `password_env` | The variable holding the IMAP and SMTP password |
| `folder` | The folder to read (default `INBOX`) |
| `poll` | The check interval when the server has no IDLE (default `1m`) |
| `allow_unauthenticated` | Accept mail without a passing DMARC or aligned DKIM result (default `false`) |

## Access

`allow_from` takes email addresses. Habakkuk ignores mail that comes from lists, auto-responders and bounces. It also ignores mail when the receiving server recorded no passing DMARC or aligned DKIM result, so a forged `From:` can't reach the agent. To turn that check off, set `allow_unauthenticated: true`.

> [!IMPORTANT]
> A session that belongs to an email channel starts **tainted**, because everything in an inbox is outside content. In a tainted turn, commands and writes outside the workspace need your approval, and the agent can't write trusted memory. See [Security](../deploy/security.md#outside-content-taints-a-turn).

## What works

- Text and attachments work in both directions.
- Each reply carries the thread's `In-Reply-To` and `References` and a `Re:` subject, so mail clients keep it in the thread.
- There's no typing indicator and there are no buttons. To answer an approval, reply **yes** or **no**.
- Habakkuk keeps its place in the mailbox (where it has read up to) in `channels/email-<name>.json`.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Your mail is ignored | Check the sender is in `allow_from`, and that your provider signs mail with DKIM and publishes DMARC. |
| Login fails | Use an app password where your provider requires one, and check `username`. |
| Replies are slow to arrive in | The server has no IDLE; lower `poll`. |
