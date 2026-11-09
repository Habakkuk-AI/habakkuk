---
title: Telegram
description: "Connect a Telegram bot in two minutes: long polling with no public URL, groups and forum topics, approval buttons, reactions, polls and voice notes."
order: 3
---

Telegram is the quickest channel after a WhatsApp agent. Habakkuk uses long polling, so it needs no public URL, and the bot gets the most complete feature set of any channel.

## Set it up

1. In Telegram, message [@BotFather](https://t.me/BotFather), send `/newbot`, pick a name, and copy the token it gives you.
2. Open your new bot and press **Start**.
3. Run `habakkuk setup` and pick **Telegram bot**. It checks the token with `getMe`, finds your Start message and lets you in. Or add the channel by hand:

```yaml
channels:
  telegram:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN
    allow_from: ["123456789"]   # your numeric user id; or leave empty and approve people with habakkuk pair
```

```bash
echo 'TELEGRAM_BOT_TOKEN=123456:ABC...' >> ~/.habakkuk/.env
habakkuk serve
```

## Access

`allow_from` takes numeric Telegram user ids. Anyone else who writes to the bot gets a pairing code to approve with `habakkuk pair CODE`. Send `/whoami` to the bot to see your id.

## Groups

In groups the bot answers mentions and replies (`groups: mention`). Forum topics are separate conversations, each with its own session.

> [!TIP]
> Telegram bots have privacy mode on by default, so in groups they only receive commands, mentions and replies. For `groups: all`, turn privacy mode off in BotFather (`/setprivacy`) and add the bot to the group again.

## What works

| Feature | Support |
|---|---|
| In | Text, photos, files, voice notes |
| Out | Text, photos up to 10 MB, documents up to 50 MB, voice notes |
| Typing indicator | Yes |
| Live progress | A draft message edited with each tool step (`progress: draft`, the default) |
| Approval buttons | Approve and Deny buttons under the request |
| Reactions | With `reactions: true`, on your message when a turn starts and ends |
| Polls | Native polls (the `poll` tool) |
| Voice replies | Voice notes, with `voice_replies: mirror` or `always` |

A `/start` reaches the agent while it has not met its user, so the first-chat welcome runs. Afterwards it shows the command list.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `telegram getMe: 401 Unauthorized` and the channel stops | The token is wrong or was revoked. Copy it again from BotFather. |
| The bot does not answer, and another program uses the same token | Only one program may poll a bot. Stop the other `habakkuk serve` or bot. Habakkuk removes a webhook set elsewhere when it starts, so polling wins. |
| `telegram bots can only download files up to 20 MB` | A Bot API limit. Send a smaller file, or a link. |
| The bot ignores group messages | Mention it or reply to it, or see the privacy mode tip above for `groups: all`. |
| The bot answers with a pairing code | You are not in `allow_from`. Run `habakkuk pair CODE` on the server. |
