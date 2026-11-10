---
title: Telegram
description: "Connect a Telegram bot in two minutes: long polling with no public URL, groups and forum topics, approval buttons, reactions, polls and voice notes."
order: 3
---

After a WhatsApp agent, a Telegram bot is the quickest channel to set up. Habakkuk uses long polling, so you don't need a public URL. The bot also gets the most complete feature set of any channel.

## Set it up

1. In Telegram, open a chat with [@BotFather](https://t.me/BotFather) and send `/newbot`. Pick a name, then copy the token it gives you.
2. Open your new bot and press **Start**.
3. Run `habakkuk setup` and pick **Telegram bot**. Setup checks the token with `getMe`, finds your Start message and lets you in. If you'd rather add the channel by hand, use this:

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

`allow_from` takes numeric Telegram user ids. Anyone else who writes to the bot gets a pairing code, which you approve with `habakkuk pair CODE`. To see your own id, send `/whoami` to the bot.

## Groups

In a group, the bot answers when you mention it or reply to it. That's the `groups: mention` setting. Each forum topic is a separate conversation with its own session.

> [!TIP]
> Telegram bots start with privacy mode on, so in groups they only receive commands, mentions and replies. If you want `groups: all`, turn privacy mode off in BotFather with `/setprivacy`. Then add the bot to the group again.

## What works

| Feature | Support |
|---|---|
| In | Text, photos, files, voice notes |
| Out | Text, photos up to 10 MB, documents up to 50 MB, voice notes |
| Typing indicator | Yes |
| Live progress | One short line such as "Checking the web", edited as the work moves on (`progress: plain`, the default); `detailed` shows a line per tool step |
| Approval buttons | Approve and Deny buttons under the request |
| Reactions | With `reactions: true`, on your message when a turn starts and ends |
| Polls | Native polls (the `poll` tool) |
| Voice replies | Voice notes, with `voice_replies: mirror` or `always` |

While the agent hasn't met its user yet, a `/start` goes through to the agent, so the first-chat welcome runs. After that, it shows the command list.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `telegram getMe: 401 Unauthorized` and the channel stops | The token is wrong or was revoked. Copy it again from BotFather. |
| The bot does not answer, and another program uses the same token | Only one program may poll a bot. Stop the other `habakkuk serve` or bot. Habakkuk removes a webhook set elsewhere when it starts, so polling wins. |
| `telegram bots can only download files up to 20 MB` | A Bot API limit. Send a smaller file, or a link. |
| The bot ignores group messages | Mention it or reply to it, or see the privacy mode tip above for `groups: all`. |
| The bot answers with a pairing code | You are not in `allow_from`. Run `habakkuk pair CODE` on the server. |
