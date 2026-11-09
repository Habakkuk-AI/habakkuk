---
title: More platforms
description: "Set up LINE, Mattermost, IRC, ntfy, Messenger, Instagram, Feishu and Lark, DingTalk, WeCom, Zulip, Rocket.Chat, Nextcloud Talk and Twitch chat."
order: 14
---

Each platform below takes its settings in a block named after its type. The rules in [Access, groups and delivery](settings.md) apply to all of them, and [What each platform supports](settings.md#what-each-platform-supports) lists what each one can send and receive.

Platforms marked **webhook** receive at `<public_url>/channels/<name>` and need an `http` section with a `public_url` the platform can reach (see [Webhook channels](settings.md#webhook-channels)). `habakkuk doctor` prints each URL.

## LINE

**Webhook.** In the LINE Developers console, create a Messaging API channel, issue a long-lived channel access token, and set the webhook URL to `<public_url>/channels/<name>`. Requests are signed with the channel secret. Replies use the event's reply token while it is fresh (free) and push messages after.

```yaml
channels:
  line:
    type: line
    line: {channel_secret_env: LINE_CHANNEL_SECRET, access_token_env: LINE_ACCESS_TOKEN}
```

Senders are LINE user ids. In groups the bot answers mentions. It sends text and JPEG and PNG images, and shows typing in one-to-one chats.

## Mattermost

Create a bot account (or a personal access token for a user) and put its token in `.env`. Habakkuk connects over the WebSocket API, so no public URL is needed.

```yaml
channels:
  mattermost:
    type: mattermost
    mattermost: {url: "https://chat.example.com", token_env: MATTERMOST_TOKEN}
```

Senders are user ids; usernames also match in `allow_from`. In channels the bot answers mentions, then follows the thread.

## IRC

```yaml
channels:
  irc:
    type: irc
    irc:
      server: irc.libera.chat:6697           # TLS by default; tls: false turns it off
      nick: habakkuk-bot
      password_env: IRC_PASSWORD             # SASL PLAIN
      channels: ["#habakkuk"]
```

Other settings: `username` (default the nick) and `sasl_user` (default the nick). A message in a channel is for Habakkuk when it has the bot's nick at the start or in the line. Senders are nicks; with SASL on the network, the account name also matches in `allow_from`. Long replies are split into lines.

## ntfy

ntfy publishes replies and notifications to a topic. With a `subscribe` topic set, messages published there become chat messages. It is handy for one-way alerts to your phone.

```yaml
channels:
  push:
    type: ntfy
    allow_from: [ntfy]                       # every ntfy message has the sender "ntfy"
    ntfy: {topic: habakkuk-out, subscribe: habakkuk-in, token_env: NTFY_TOKEN}
```

`url` defaults to `https://ntfy.sh`; set it for your own server. `token_env` is for protected topics. `subscribe` may equal `topic`.

> [!CAUTION]
> Anyone who knows an unprotected topic name can publish to it. Use a protected topic (with `token_env`) or a long random name for `subscribe`.

## Messenger and Instagram

**Webhook.** Both use a Meta app, like the [WhatsApp Business Cloud API](whatsapp.md#business-cloud-api). In the app, subscribe the webhook to `<public_url>/channels/<name>` with a verify token of your choice (put it in the `verify_token_env` variable) and the `messages` field. Requests are checked against `X-Hub-Signature-256` with the app secret. Both use a Page access token.

```yaml
channels:
  fbpage:
    type: messenger
    messenger: {access_token_env: FB_PAGE_TOKEN, app_secret_env: META_APP_SECRET, verify_token_env: META_VERIFY_TOKEN}
  insta:
    type: instagram
    instagram: {access_token_env: IG_PAGE_TOKEN, app_secret_env: META_APP_SECRET, verify_token_env: META_VERIFY_TOKEN}
```

`page_id` (optional) is the Page or Instagram account id, to ignore events for others. Replies outside the platforms' 24-hour window fail with the reason, since Habakkuk does not use message tags such as `human_agent`. Approvals show as quick replies. Instagram fetches the files Habakkuk sends from one-hour URLs under the channel's path.

## Feishu and Lark

**Webhook.** Create a custom app with the bot capability, add the `im:message` permissions, and subscribe to `im.message.receive_v1` with the request URL `<public_url>/channels/<name>`. Set an Encrypt Key and put it in `encrypt_key_env`, so events arrive encrypted and signed.

```yaml
channels:
  feishu:
    type: feishu
    feishu:
      app_id: cli_a1b2c3d4e5
      app_secret_env: FEISHU_APP_SECRET
      verification_token_env: FEISHU_VERIFICATION_TOKEN
      encrypt_key_env: FEISHU_ENCRYPT_KEY
      # domain: lark                 # for Lark (open.larksuite.com); default feishu
      # reply_in_thread: true        # answer each group mention in a thread under it
```

Senders are open ids (`ou_...`). In groups the bot answers mentions. It sends Markdown posts, images, files and voice.

## DingTalk

**Webhook.** Add a robot to an internal app, choose HTTP mode, and set the message receiving address to `<public_url>/channels/<name>`.

```yaml
channels:
  dingtalk:
    type: dingtalk
    dingtalk: {client_id: dingabcdef123456, client_secret_env: DINGTALK_CLIENT_SECRET}
```

`client_id` is the app's AppKey and `client_secret_env` its AppSecret; `robot_code` defaults to `client_id`. Senders are staff ids. DingTalk only sends the robot group messages that mention it.

## WeCom

**Webhook.** Set the application's API receiving URL to `<public_url>/channels/<name>` with a Token and EncodingAESKey, and add the server's public IP to the app's trusted IPs.

```yaml
channels:
  wecom:
    type: wecom
    wecom: {corp_id: ww0123456789abcdef, agent_id: 1000002, secret_env: WECOM_SECRET, token_env: WECOM_TOKEN, aes_key_env: WECOM_AES_KEY}
```

Senders are member user ids. `markdown: true` sends Markdown, which WeChat users see as plain text.

## Zulip

Create a generic bot, put its API key in `.env`, and subscribe it to the streams it should read. Habakkuk long-polls the event queue, so no public URL is needed.

```yaml
channels:
  zulip:
    type: zulip
    zulip: {url: "https://example.zulipchat.com", email: habakkuk-bot@example.zulipchat.com, api_key_env: ZULIP_API_KEY}
```

A stream is the conversation `stream:<id>` and each topic a thread. Direct messages are `dm:<user ids>`. Senders are user ids. In streams the bot answers mentions.

## Rocket.Chat

Create a bot user and make it a personal access token (Profile > Personal Access Tokens). Set `user_id` and `token_env` from it.

```yaml
channels:
  rocketchat:
    type: rocketchat
    rocketchat: {url: "https://chat.example.com", user_id: aobEdbYhXfu5hkeqG, token_env: ROCKETCHAT_TOKEN}
```

The bot follows every room it is in over the realtime API. Rooms are conversations and threads are threads. It answers mentions and replies in threads.

## Nextcloud Talk

**Webhook.** Install a bot with a 40 to 128 character secret, also put in `secret_env`:

```bash
occ talk:bot:install --feature webhook --feature response --feature reaction Habakkuk <secret> <public_url>/channels/<name>
```

Enable it in the rooms it should read.

```yaml
channels:
  talk:
    type: nextcloudtalk
    groups: all
    nextcloudtalk: {url: "https://cloud.example.com", secret_env: TALK_BOT_SECRET}
```

Talk does not tell bots which rooms are one-to-one, so a message is for Habakkuk when it says `@Habakkuk` (`mention`), or set `groups: all`. Bots cannot send files. Reactions work.

## Twitch

Make an account for the bot and a user access token for it with the `chat:read` and `chat:edit` scopes, and list the channels to join.

```yaml
channels:
  twitch:
    type: twitch
    allow_from: ["*"]                       # chat is public; or list user ids
    twitch: {login: habakkuk_bot, token_env: TWITCH_TOKEN, channels: [mystream]}
```

Chat is public, so only messages naming the bot (`@habakkuk_bot ...` or `habakkuk_bot: ...`) reach the agent. Senders are numeric user ids, and replies are paced under Twitch's rate limit.

> [!WARNING]
> With `allow_from: ["*"]`, every chatter is a guest: no commands, no writes outside the workspace, no computer use. Keep it that way for a public chat.
