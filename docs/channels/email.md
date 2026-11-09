---
title: Email
description: "Give Habakkuk a mailbox: IMAP with IDLE or polling, SMTP replies that keep the thread, sender checks, and why email sessions start tainted."
order: 8
---

Habakkuk reads a mailbox over IMAP (IDLE when the server has it, else polling) and answers over SMTP. Each thread is a conversation, and replies keep it together. No public URL is needed.

## Set it up

1. Make a mailbox for the agent, and an app password if your provider uses them.
2. Put the password in `.env` and configure the channel:

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

`allow_from` takes email addresses. Mail is ignored when it comes from lists, auto-responders and bounces, and when the receiving server recorded no passing DMARC or aligned DKIM result. That keeps a forged `From:` from reaching the agent. `allow_unauthenticated: true` turns the check off.

> [!IMPORTANT]
> A session bound to an email channel starts **tainted**, since everything in an inbox is outside content. In a tainted turn, commands and writes outside the workspace need your approval, and the agent cannot write trusted memory. See [Security](../deploy/security.md#outside-content-taints-a-turn).

## What works

- Text and attachments in and out.
- Replies keep the thread's `In-Reply-To` and `References` and a `Re:` subject, so mail clients thread them.
- No typing indicator or buttons: approvals are answered by replying **yes** or **no**.
- Where the mailbox was read up to is kept in `channels/email-<name>.json`.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Your mail is ignored | Check the sender is in `allow_from`, and that your provider signs mail with DKIM and publishes DMARC. |
| Login fails | Use an app password where your provider requires one, and check `username`. |
| Replies are slow to arrive in | The server has no IDLE; lower `poll`. |
