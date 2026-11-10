---
title: Channels
description: "Every chat app Habakkuk can talk on, how each one connects, how much setup it takes, and which one to start with."
order: 2
---

Every chat app Habakkuk can talk on is listed below, with advice on which one to start with. A channel is one account on one chat platform, such as a Telegram bot, a Slack app or a WhatsApp agent. You define channels under `channels:` in `config.yaml`. Each one sits under an account name you choose, and has a `type`. `hk serve` runs them. To see their health, run `hk channels` or send `/status` in a chat.

```yaml
channels:
  telegram:                   # the account name: yours to choose
    type: telegram            # the platform
    token_env: TELEGRAM_BOT_TOKEN
```

> [!TIP]
> **Start with a WhatsApp agent or Telegram.** Both connect by polling, so you don't need a public URL, and `hk setup` checks the key or token for you. A WhatsApp agent takes about a minute to set up. A Telegram bot takes two.

## All channels

In the table, "Public URL" means the platform calls Habakkuk over the internet. For those channels, `hk serve` needs an HTTPS address the platform can reach. See [Webhook channels](settings.md#webhook-channels).

| Channel | `type` | Connects by | Public URL | Setup |
|---|---|---|---|---|
| [WhatsApp agent](whatsapp.md#whatsapp-agents) | `whatsappagent` | long polling | no | easy, in `hk setup` |
| [WhatsApp linked device](whatsapp.md#linked-device) | `whatsapp` | a linked device, like WhatsApp Web | no | easy, in `hk setup` |
| [WhatsApp Business Cloud API](whatsapp.md#business-cloud-api) | `whatsappcloud` | webhook + Graph API | yes | involved: a Meta app and business account |
| [Telegram](telegram.md) | `telegram` | long polling | no | easy, in `hk setup` |
| [Slack](slack.md) | `slack` | Socket Mode | no | moderate: a Slack app, in `hk setup` |
| [Discord](discord.md) | `discord` | Gateway websocket | no | moderate: a bot application, in `hk setup` |
| [Signal](signal.md) | `signal` | a signal-cli daemon | no | moderate: run signal-cli |
| [Matrix](matrix.md) | `matrix` | `/sync` long polling | no | moderate: a bot account and token |
| [Email](email.md) | `email` | IMAP (IDLE or polling) + SMTP | no | easy: a mailbox and an app password |
| [SMS](sms.md) | `sms` | Twilio webhook + REST | yes | moderate: a Twilio number |
| [Microsoft Teams](microsoft-teams.md) | `msteams` | Bot Framework webhook | yes | involved: an Azure bot registration |
| [Google Chat](google-chat.md) | `googlechat` | Chat app HTTP endpoint | yes | involved: a Google Cloud project |
| [iMessage](imessage.md) | `bluebubbles` | a BlueBubbles server on a Mac | yes | moderate: a Mac that stays on |
| [Web chat](web-chat.md) | `web` | Habakkuk's own HTTP server | no | easy, in `hk setup` |
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

## What has been tested live

So far, three channels have run against the real service:

- **WhatsApp linked device** (`whatsapp`), linked to a real account.
- **WhatsApp agents** (`whatsappagent`).
- **Slack** over Socket Mode (`slack`), including two Slack apps running side by side.

The web chat is Habakkuk's own HTTP server, so its tests already run against the real thing.

Every other channel in the table is built from the platform's documented API, and tested here against a fake platform server. None of them has run against the real service yet, so expect rough edges. If you hit one, please [report what you find](https://github.com/habakkuk-ai/habakkuk/issues/new/choose) and include the output of `hk logs -level warn -since 1h`.

## How channels work

- **Who may talk.** When someone who isn't in `allow_from` sends a direct message, they get a pairing code. You approve it with `hk pair CODE`. See [Access, groups and delivery](settings.md#who-may-talk-to-the-bot).
- **Groups.** In a group, the bot answers only when you address it. That means a mention, a reply, or the platform's equivalent. With `groups: all` it answers everything, and with `off` it ignores groups.
- **One conversation, one session.** Each chat is a conversation with its own session. So is each thread, on platforms that have threads. A fresh session starts after 24 hours of silence, or when you send `/new`.
- **Exactly once.** If a platform delivers a message again, it doesn't run a second time. Each reply is delivered once. After a crash, a long reply resumes at the first chunk that wasn't sent.
- **Replies keep their context.** When you reply to a message (quote it), the agent sees what you quoted. Some messages reach the chat from elsewhere, such as a scheduled task's result or another agent's question. The agent sees those on your next message. See [Access, groups and delivery](settings.md#replies-and-quotes) and [Sessions](../agents/sessions.md#messages-from-elsewhere-in-the-same-chat).
- **Supervised.** Every adapter runs under a supervisor. The supervisor restarts the adapter with backoff, and tears down a connection that stays silent too long.
- **Several at once.** You can run as many channels as you like, of the same type or different types. You can also route each channel, or each conversation, to a different [agent](../agents/multi-agent.md#who-answers-a-conversation).

## Next steps

<!-- cards -->
- [Access, groups and delivery](settings.md): pairing, group rules, webhooks, the settings every channel takes, and what each platform supports.
- [WhatsApp](whatsapp.md): the three ways to be on WhatsApp, compared.
- [Telegram](telegram.md): a bot in two minutes, with buttons, reactions and voice notes.
