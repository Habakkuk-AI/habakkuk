---
title: Channels
description: "Every chat app Habakkuk can talk on, how each one connects, how much setup it takes, and which one to start with."
order: 2
---

A channel is one account on one chat platform: a Telegram bot, a Slack app, a WhatsApp agent. Channels live under `channels:` in `config.yaml`, keyed by an account name you choose, with a `type`. `habakkuk serve` runs them, and `habakkuk channels` (or `/status` in a chat) shows their health.

```yaml
channels:
  telegram:                   # the account name: yours to choose
    type: telegram            # the platform
    token_env: TELEGRAM_BOT_TOKEN
```

> [!TIP]
> **Start with a WhatsApp agent or Telegram.** Both connect by polling, so they need no public URL, and `habakkuk setup` checks the key or token for you. A WhatsApp agent takes about a minute; a Telegram bot takes two.

## All channels

"Public URL" means the platform calls Habakkuk over the internet, so `habakkuk serve` needs an HTTPS address it can reach (see [Webhook channels](settings.md#webhook-channels)).

| Channel | `type` | Connects by | Public URL | Setup |
|---|---|---|---|---|
| [WhatsApp agent](whatsapp.md#whatsapp-agents) | `whatsappagent` | long polling | no | easy, in `habakkuk setup` |
| [WhatsApp linked device](whatsapp.md#linked-device) | `whatsapp` | a linked device, like WhatsApp Web | no | easy, in `habakkuk setup` |
| [WhatsApp Business Cloud API](whatsapp.md#business-cloud-api) | `whatsappcloud` | webhook + Graph API | yes | involved: a Meta app and business account |
| [Telegram](telegram.md) | `telegram` | long polling | no | easy, in `habakkuk setup` |
| [Slack](slack.md) | `slack` | Socket Mode | no | moderate: a Slack app, in `habakkuk setup` |
| [Discord](discord.md) | `discord` | Gateway websocket | no | moderate: a bot application, in `habakkuk setup` |
| [Signal](signal.md) | `signal` | a signal-cli daemon | no | moderate: run signal-cli |
| [Matrix](matrix.md) | `matrix` | `/sync` long polling | no | moderate: a bot account and token |
| [Email](email.md) | `email` | IMAP (IDLE or polling) + SMTP | no | easy: a mailbox and an app password |
| [SMS](sms.md) | `sms` | Twilio webhook + REST | yes | moderate: a Twilio number |
| [Microsoft Teams](microsoft-teams.md) | `msteams` | Bot Framework webhook | yes | involved: an Azure bot registration |
| [Google Chat](google-chat.md) | `googlechat` | Chat app HTTP endpoint | yes | involved: a Google Cloud project |
| [iMessage](imessage.md) | `bluebubbles` | a BlueBubbles server on a Mac | yes | moderate: a Mac that stays on |
| [Web chat](web-chat.md) | `web` | Habakkuk's own HTTP server | no | easy, in `habakkuk setup` |
| [LINE](more-platforms.md#line) | `line` | webhook | yes | moderate |
| [Mattermost](more-platforms.md#mattermost) | `mattermost` | WebSocket + REST | no | easy |
| [IRC](more-platforms.md#irc) | `irc` | TLS, SASL PLAIN | no | easy |
| [ntfy](more-platforms.md#ntfy) | `ntfy` | JSON stream + publish | no | easy |
| [Messenger](more-platforms.md#messenger-and-instagram) | `messenger` | webhook + Graph API | yes | involved: a Meta app |
| [Instagram](more-platforms.md#messenger-and-instagram) | `instagram` | webhook + Graph API | yes | involved: a Meta app |
| [Feishu / Lark](more-platforms.md#feishu-and-lark) | `feishu` | event subscription + IM API | yes | moderate |
| [DingTalk](more-platforms.md#dingtalk) | `dingtalk` | robot HTTP callback | yes | moderate |
| [WeCom](more-platforms.md#wecom) | `wecom` | app callback + message API | yes | moderate |
| [Zulip](more-platforms.md#zulip) | `zulip` | event queue long-poll + REST | no | easy |
| [Rocket.Chat](more-platforms.md#rocketchat) | `rocketchat` | realtime API + REST | no | easy |
| [Nextcloud Talk](more-platforms.md#nextcloud-talk) | `nextcloudtalk` | Talk bot webhook | yes | moderate |
| [Twitch chat](more-platforms.md#twitch) | `twitch` | IRC over TLS | no | easy |

## How channels work

- **Who may talk.** Direct messages from someone not in `allow_from` get a pairing code that you approve with `habakkuk pair CODE`. See [Access, groups and delivery](settings.md#who-may-talk-to-the-bot).
- **Groups.** In groups the bot answers only when addressed (a mention, a reply, or the platform's equivalent). `groups: all` answers everything; `off` ignores groups.
- **One conversation, one session.** Each chat (and each thread, where the platform has them) is a conversation with its own session. A fresh session starts after 24 hours of silence or when you send `/new`.
- **Exactly once.** A redelivered message never runs twice, a reply is delivered once, and a long reply resumes at the first unsent chunk after a crash.
- **Supervised.** Every adapter runs under a supervisor that restarts it with backoff and tears down a connection that stays silent too long.
- **Several at once.** Run as many channels as you like, of the same or different types, and route each one (or each conversation) to a different [agent](../agents/multi-agent.md#who-answers-a-conversation).

## Next steps

<!-- cards -->
- [Access, groups and delivery](settings.md): pairing, group rules, webhooks, the settings every channel takes, and what each platform supports.
- [WhatsApp](whatsapp.md): the three ways to be on WhatsApp, compared.
- [Telegram](telegram.md): a bot in two minutes, with buttons, reactions and voice notes.
