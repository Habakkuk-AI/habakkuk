---
title: Discord
description: "Connect a Discord bot over the Gateway: the Message Content intent, invite permissions, servers and threads, buttons, reactions and polls."
order: 5
---

Habakkuk talks on Discord through a bot you create. The bot connects over the Gateway websocket, with resume, so you don't need a public URL.

## Set it up

1. Go to the [Discord developer portal](https://discord.com/developers/applications). Create an application and add a bot to it.
2. In the bot's settings, turn on the **Message Content** intent.
3. Copy the bot's token.
4. Invite the bot to your server. Under OAuth2 > URL Generator, choose the `bot` scope and these permissions: View Channels, Send Messages, Send Messages in Threads, Read Message History and Attach Files.
5. Run `hk setup` and pick **Discord bot**. It checks the token for you. Or add the channel by hand:

```yaml
channels:
  discord:
    type: discord
    token_env: DISCORD_BOT_TOKEN
```

## Access

`allow_from` takes Discord user ids. To find one, turn on Developer Mode in Discord, then right-click a user and copy the id. Anyone else who sends the bot a direct message gets a pairing code.

## Servers and threads

In a server, the bot answers when you mention it or reply to it. Each thread is its own conversation. Once the bot has replied in a thread, it treats later messages there as addressed to it.

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
| `discord: the Message Content intent is not enabled for this bot in the developer portal` | Switch the intent on, then restart `serve`. `hk channels` shows this too. |
| `discord: authentication failed (check the bot token)` | Copy the token again, or reset it in the portal. |
| The bot is online but silent in a channel | Check it has View Channels and Read Message History there, and mention it. |
