---
title: Slack
description: "Connect a Slack app over Socket Mode: one connection for every workspace, mentions and threads, files, reactions and approval buttons, with no public URL."
order: 4
---

Habakkuk talks on Slack through an app you create. The app connects over Socket Mode, so you don't need a public URL. One app-level token holds one connection for every workspace the app is installed in. Each workspace also has its own bot token.

## Set it up

1. Create an app at [api.slack.com/apps](https://api.slack.com/apps) and turn on **Socket Mode**. Then create an app-level token with the `connections:write` scope. It looks like `xapp-...`.
2. Under **Event Subscriptions**, subscribe to the bot events `app_mention` and `message.im`. Add `message.channels` and `message.groups` too, so the bot can follow threads it was mentioned in. Then add the OAuth scopes those events need, plus `chat:write`.
3. If you want files and reactions, add `files:read`, `files:write` and `reactions:write`. If you want approval buttons, turn on **Interactivity**.
4. Install the app to your workspace and copy the bot token, which looks like `xoxb-...`.
5. Run `habakkuk setup` and pick **Slack app**. It checks both tokens for you. Or add the channel by hand:

```yaml
channels:
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN              # Socket Mode, one connection for every workspace
    bot_tokens_env: [SLACK_BOT_TOKEN, SLACK_BOT_TOKEN_2]   # one per workspace
    routes: {T0123456: ops}                     # a workspace or conversation can go to another agent
```

## Several Slack apps

When one app is installed in several workspaces, you need one channel. List a bot token for each workspace in `bot_tokens_env`. It's different when each workspace has its own app, for example an app you made in your workspace and another made in a client's. In that case, add one `type: slack` channel per app. Each channel has its own app token and its own Socket Mode connection, and they run side by side in one `habakkuk serve`. Both can go to the same agent:

```yaml
channels:
  slack:
    type: slack
    agent: habakkuk
    app_token_env: SLACK_APP_TOKEN
    bot_tokens_env: [SLACK_BOT_TOKEN]
    allow_from: [U0123ABCD]
  slack-acme:
    type: slack
    agent: habakkuk                     # the same agent, or another
    app_token_env: ACME_SLACK_APP_TOKEN
    bot_tokens_env: [ACME_SLACK_BOT_TOKEN]
    allow_from: [U0999WXYZ]             # user ids from that workspace
    listen_channels: [C0777GHI]
```

Each channel has its own `allow_from`, pairing, routes and settings, and its own conversations. `habakkuk setup` edits the first Slack channel. You add the second one by hand, then put its tokens in `.env`.

## Access

`allow_from` takes Slack user ids, such as `U0123ABCD`. Anyone else who sends the bot a direct message gets a pairing code. `routes` sends a workspace (by its team id) or a single conversation to another agent. That way one app can serve several teams with different agents.

## Channels and threads

In a channel, the bot answers when you mention it. Its reply goes in a thread under your mention. Once it has replied in a thread, it treats later messages there as addressed to it.

In a direct message, the bot answers at the top level of the chat, even when you write inside a thread there.

### Channels where no mention is needed

`listen_channels` names the channels where the bot treats every message from an allowed sender as addressed to it. You don't need an `@mention` there:

```yaml
channels:
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN
    bot_tokens_env: [SLACK_BOT_TOKEN]
    allow_from: [U0123ABCD, U0456EFGH]
    listen_channels: [C0123ABC, C0456DEF]
```

- Use each channel's id rather than its name. To find it in Slack, open the channel's name, then **About**. The id is at the bottom. If you have several workspaces, write an entry as `T0123/C0123ABC` to limit it to one workspace.
- A top-level message starts a thread under it, and the bot answers in that thread. A message inside a thread continues that thread, the same as with a mention.
- The bot answers only senders who are in `allow_from` or paired. It ignores everyone else and doesn't send them a pairing code. It ignores other bots too.
- Invite the bot to each channel. Subscribe to `message.channels` with the `channels:history` scope. For private channels, also subscribe to `message.groups` with the `groups:history` scope.

## What works

| Feature | Support |
|---|---|
| In | Text, files (needs `files:read`) |
| Out | Text, files up to 100 MB (needs `files:write`) |
| Live progress | One short line such as "Checking the web", edited as the work moves on (`progress: plain`, the default); `detailed` shows a line per tool step |
| Typing indicator | Slack has none for bots. With `progress: off`, a turn that runs past 60 seconds posts one "still working" notice |
| Buttons | Approve and Deny on approval requests. A press is acknowledged to Slack at once. A button on a request from before a restart answers the request asked again only when it is the same call; otherwise you are told it expired |
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
