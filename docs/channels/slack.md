---
title: Slack
description: "Connect a Slack app over Socket Mode: one connection for every workspace, mentions and threads, files, reactions and approval buttons, with no public URL."
order: 4
---

Slack connects over Socket Mode. One app-level token holds one connection for every workspace the app is installed in, with a bot token per workspace. No public URL is needed.

## Set it up

1. Create an app at [api.slack.com/apps](https://api.slack.com/apps) and turn on **Socket Mode**. Create an app-level token (`xapp-...`) with the `connections:write` scope.
2. Under **Event Subscriptions**, subscribe to the bot events `app_mention` and `message.im`, plus `message.channels` and `message.groups` so the bot can follow threads it was mentioned in. Add the OAuth scopes those events need, and `chat:write`.
3. For files and reactions, add `files:read`, `files:write` and `reactions:write`. For approval buttons, turn on **Interactivity**.
4. Install the app to your workspace and copy the bot token (`xoxb-...`).
5. Run `habakkuk setup` and pick **Slack app** (it checks both tokens), or add the channel by hand:

```yaml
channels:
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN              # Socket Mode, one connection for every workspace
    bot_tokens_env: [SLACK_BOT_TOKEN, SLACK_BOT_TOKEN_2]   # one per workspace
    routes: {T0123456: ops}                     # a workspace or conversation can go to another agent
```

## Access

`allow_from` takes Slack user ids (`U0123ABCD`). Anyone else who sends a direct message gets a pairing code. `routes` maps a workspace (team id) or a conversation to another agent, so one app can serve several teams with different agents.

## Channels and threads

In channels the bot answers mentions and replies in a thread under the mention. Once it has replied in a thread, later messages there count as addressed to it.

Direct messages are answered at the top level of the chat, even when you write inside a thread there.

### Channels where no mention is needed

`listen_channels` lists channels where every message from an allowed sender is for the bot, with no `@mention`:

```yaml
channels:
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN
    bot_tokens_env: [SLACK_BOT_TOKEN]
    allow_from: [U0123ABCD, U0456EFGH]
    listen_channels: [C0123ABC, C0456DEF]
```

- Use channel ids, not names. In Slack, open the channel's name, then **About**: the id is at the bottom. With several workspaces, `T0123/C0123ABC` limits an entry to one workspace.
- A top-level message starts a thread under it, and the bot answers there. A message inside a thread continues that thread, as with a mention.
- Only senders in `allow_from` (or paired) are answered. Others are ignored without a pairing code, and so are other bots.
- Invite the bot to each channel, and subscribe to `message.channels` (and `message.groups` for private channels) with the `channels:history` (and `groups:history`) scope.

## What works

| Feature | Support |
|---|---|
| In | Text, files (needs `files:read`) |
| Out | Text, files up to 100 MB (needs `files:write`) |
| Live progress | A draft message edited with each tool step (`progress: draft`, the default) |
| Typing indicator | Slack has none for bots. With `progress: off`, a turn that runs past 60 seconds posts one "still working" notice |
| Approval buttons | Approve and Deny, with Interactivity turned on |
| Reactions | With `reactions: true` (needs `reactions:write`) |

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `slack download returned a web page: the app needs the files:read scope` | Add `files:read`, reinstall the app, and update the bot token. |
| `... (the Slack app needs the files:write scope)` | Add `files:write`, reinstall the app, and update the bot token. |
| `slack: no bot token for workspace T...` | The app is installed in a workspace whose bot token is not in `bot_tokens_env`. Add it. |
| The bot ignores follow-ups in a thread | Subscribe to `message.channels` (and `message.groups` for private channels). |
| The bot never answers in channels | Invite it to the channel, and mention it. |
