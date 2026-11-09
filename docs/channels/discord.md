---
title: Discord
description: "Connect a Discord bot over the Gateway: the Message Content intent, invite permissions, servers and threads, buttons, reactions and polls."
order: 5
---

Discord connects over the Gateway websocket, with resume. No public URL is needed.

## Set it up

1. At the [Discord developer portal](https://discord.com/developers/applications), create an application and add a bot.
2. Under the bot's settings, switch on the **Message Content** intent.
3. Copy the bot's token.
4. Invite the bot to your server (OAuth2 > URL Generator) with the `bot` scope and these permissions: View Channels, Send Messages, Send Messages in Threads, Read Message History and Attach Files.
5. Run `habakkuk setup` and pick **Discord bot** (it checks the token), or add the channel by hand:

```yaml
channels:
  discord:
    type: discord
    token_env: DISCORD_BOT_TOKEN
```

## Access

`allow_from` takes Discord user ids (enable Developer Mode in Discord, then right-click a user and copy the id). Anyone else who sends a direct message gets a pairing code.

## Servers and threads

In servers the bot answers mentions and replies. A thread is its own conversation; once the bot has replied in a thread, later messages there count as addressed to it.

## What works

| Feature | Support |
|---|---|
| In | Text, files |
| Out | Text, files up to 10 MB |
| Typing indicator | Yes |
| Approval buttons | Yes |
| Reactions | With `reactions: true` |
| Polls | Native polls (the `poll` tool) |

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `discord: the Message Content intent is not enabled for this bot in the developer portal` | Switch the intent on, then restart `serve`. `habakkuk channels` shows this too. |
| `discord: authentication failed (check the bot token)` | Copy the token again, or reset it in the portal. |
| The bot is online but silent in a channel | Check it has View Channels and Read Message History there, and mention it. |
