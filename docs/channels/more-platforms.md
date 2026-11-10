---
title: More platforms
description: "Set up LINE, Mattermost, IRC, ntfy, Messenger, Instagram, Feishu and Lark, DingTalk, WeCom, Zulip, Rocket.Chat, Nextcloud Talk and Twitch chat."
order: 14
---

The platforms that don't have a page of their own are set up here, one short section each. Each platform takes its settings in a block named after its type. The rules in [Access, groups and delivery](settings.md) apply to all of them. For what each one can send and receive, see [What each platform supports](settings.md#what-each-platform-supports).

A platform marked **webhook** receives at `<public_url>/channels/<name>`. It needs an `http` section with a `public_url` the platform can reach. See [Webhook channels](settings.md#webhook-channels). Run `habakkuk doctor` to print each URL.

## LINE

**Webhook.** In the LINE Developers console, create a Messaging API channel. Issue a long-lived channel access token, then set the webhook URL to `<public_url>/channels/<name>`. Requests are signed with the channel secret. Habakkuk replies with the event's reply token while the token is fresh, which is free. After that it sends push messages.

```yaml
channels:
  line:
    type: line
    line: {channel_secret_env: LINE_CHANNEL_SECRET, access_token_env: LINE_ACCESS_TOKEN}
```

Senders are identified by their LINE user id. In groups, the bot answers when you mention it. It sends text, plus JPEG and PNG images. In one-to-one chats it shows typing.

## Mattermost

Create a bot account, or a personal access token for a user. Put the token in `.env`. Habakkuk connects over the WebSocket API, so you don't need a public URL.

```yaml
channels:
  mattermost:
    type: mattermost
    mattermost: {url: "https://chat.example.com", token_env: MATTERMOST_TOKEN}
```

Senders are identified by user id, and a username matches in `allow_from` too. In channels, the bot answers when you mention it, then follows the thread.

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

There are two other settings, `username` and `sasl_user`. Both default to the nick. In a channel, Habakkuk treats a message as addressed to it when the bot's nick is at the start of the line or somewhere in it. Senders are identified by nick. With SASL on the network, the account name matches in `allow_from` too. Habakkuk splits long replies into lines.

## ntfy

The ntfy channel publishes replies and notifications to a topic. If you also set a `subscribe` topic, messages published there come in as chat messages. It's handy for one-way alerts to your phone.

```yaml
channels:
  push:
    type: ntfy
    allow_from: [ntfy]                       # every ntfy message has the sender "ntfy"
    ntfy: {topic: habakkuk-out, subscribe: habakkuk-in, token_env: NTFY_TOKEN}
```

`url` defaults to `https://ntfy.sh`. Set it when you use your own server. `token_env` is for protected topics. `subscribe` can be the same as `topic`.

> [!CAUTION]
> Anyone who knows the name of an unprotected topic can publish to it. For `subscribe`, use a protected topic (with `token_env`) or a long random name.

## Messenger and Instagram

**Webhook.** Both use a Meta app, the same way the [WhatsApp Business Cloud API](whatsapp.md#business-cloud-api) does. In the app, subscribe the webhook to `<public_url>/channels/<name>` with the `messages` field. Choose a verify token and put it in the `verify_token_env` variable. Habakkuk checks each request against `X-Hub-Signature-256`, using the app secret. Both platforms use a Page access token.

```yaml
channels:
  fbpage:
    type: messenger
    messenger: {access_token_env: FB_PAGE_TOKEN, app_secret_env: META_APP_SECRET, verify_token_env: META_VERIFY_TOKEN}
  insta:
    type: instagram
    instagram: {access_token_env: IG_PAGE_TOKEN, app_secret_env: META_APP_SECRET, verify_token_env: META_VERIFY_TOKEN}
```

`page_id` is optional. Set it to the Page or Instagram account id, and Habakkuk ignores events for others. A reply outside the platforms' 24-hour window fails and gives the reason, because Habakkuk doesn't use message tags such as `human_agent`. Approvals show as quick replies. Instagram fetches the files Habakkuk sends from URLs under the channel's path. Each URL lasts one hour.

## Feishu and Lark

**Webhook.** Create a custom app with the bot capability and add the `im:message` permissions. Subscribe to `im.message.receive_v1`, with `<public_url>/channels/<name>` as the request URL. Then set an Encrypt Key and put it in `encrypt_key_env`. That way events arrive encrypted and signed.

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

Senders are identified by open id, which looks like `ou_...`. In groups, the bot answers when you mention it. It sends Markdown posts, images, files and voice.

## DingTalk

**Webhook.** Add a robot to an internal app and choose HTTP mode. Then set the message receiving address to `<public_url>/channels/<name>`.

```yaml
channels:
  dingtalk:
    type: dingtalk
    dingtalk: {client_id: dingabcdef123456, client_secret_env: DINGTALK_CLIENT_SECRET}
```

`client_id` is the app's AppKey, and `client_secret_env` is its AppSecret. `robot_code` defaults to `client_id`. Senders are identified by staff id. In groups, DingTalk only sends the robot the messages that mention it.

## WeCom

**Webhook.** Set the application's API receiving URL to `<public_url>/channels/<name>`, with a Token and an EncodingAESKey. Then add the server's public IP to the app's trusted IPs.

```yaml
channels:
  wecom:
    type: wecom
    wecom: {corp_id: ww0123456789abcdef, agent_id: 1000002, secret_env: WECOM_SECRET, token_env: WECOM_TOKEN, aes_key_env: WECOM_AES_KEY}
```

Senders are identified by member user id. With `markdown: true`, Habakkuk sends Markdown. WeChat users see it as plain text.

## Zulip

Create a generic bot and put its API key in `.env`. Subscribe the bot to the streams you want it to read. Habakkuk long-polls the event queue, so you don't need a public URL.

```yaml
channels:
  zulip:
    type: zulip
    zulip: {url: "https://example.zulipchat.com", email: habakkuk-bot@example.zulipchat.com, api_key_env: ZULIP_API_KEY}
```

Each stream is the conversation `stream:<id>`, and each topic in it is a thread. Direct messages are the conversation `dm:<user ids>`. Senders are identified by user id. In streams, the bot answers when you mention it.

## Rocket.Chat

Create a bot user. Then make a personal access token for it, under Profile > Personal Access Tokens. Use it to set `user_id` and `token_env`.

```yaml
channels:
  rocketchat:
    type: rocketchat
    rocketchat: {url: "https://chat.example.com", user_id: aobEdbYhXfu5hkeqG, token_env: ROCKETCHAT_TOKEN}
```

The bot follows every room it's in, over the realtime API. Each room is a conversation, and threads stay threads. The bot answers when you mention it, and it replies in threads.

## Nextcloud Talk

**Webhook.** Install a bot with a secret that's 40 to 128 characters long. Put the same secret in `secret_env`:

```bash
occ talk:bot:install --feature webhook --feature response --feature reaction Habakkuk <secret> <public_url>/channels/<name>
```

Then enable the bot in each room you want it to read.

```yaml
channels:
  talk:
    type: nextcloudtalk
    groups: all
    nextcloudtalk: {url: "https://cloud.example.com", secret_env: TALK_BOT_SECRET}
```

Talk doesn't tell a bot which rooms are one-to-one. So Habakkuk treats a message as addressed to it when the message says `@Habakkuk` (the `mention` setting). Or you can set `groups: all`. Bots can't send files. Reactions work.

## Twitch

Create an account for the bot. Make a user access token for it with the `chat:read` and `chat:edit` scopes. Then list the channels you want it to join.

```yaml
channels:
  twitch:
    type: twitch
    allow_from: ["*"]                       # chat is public; or list user ids
    twitch: {login: habakkuk_bot, token_env: TWITCH_TOKEN, channels: [mystream]}
```

Twitch chat is public, so the agent only gets messages that name the bot, written as `@habakkuk_bot ...` or `habakkuk_bot: ...`. Senders are identified by numeric user id. Habakkuk paces its replies to stay under Twitch's rate limit.

> [!WARNING]
> With `allow_from: ["*"]`, every chatter is a guest. A guest gets no commands, no writes outside the workspace and no computer use. Keep it that way for a public chat.
