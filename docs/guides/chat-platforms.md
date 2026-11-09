---
title: Telegram, Slack, Discord and more
description: "Set up every channel type other than WhatsApp: Telegram, Slack, Discord, Signal, Matrix, email, SMS, Teams, Google Chat, iMessage, LINE and the rest."
order: 3
---

The [channels overview](channels.md) has the capability tables and the settings every channel shares. This page covers each platform's own setup. WhatsApp has [its own page](whatsapp.md).

## Telegram

Create a bot with BotFather and put its token in the variable `token_env` names. Telegram uses long polling, so no public URL is needed.

```yaml
channels:
  telegram:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN
    allow_from: ["123456789"]   # your numeric user id; or leave empty and approve people with habakkuk pair
```

In groups the bot answers mentions and replies; forum topics are separate conversations. Telegram gets typing, live progress drafts, approval buttons, reactions, native polls and voice notes. A `/start` reaches the agent while it has not met its user, and shows the command list afterwards.

## Slack

Slack connects over Socket Mode: one app-level token (`app_token_env`) holds one connection for every workspace the app is installed in, with a bot token per workspace in `bot_tokens_env`. No public URL is needed.

```yaml
channels:
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN              # Socket Mode, one connection for every workspace
    bot_tokens_env: [SLACK_BOT_TOKEN, SLACK_BOT_TOKEN_2]
    routes: {T0123456: ops}                     # a workspace or conversation can go to another agent
```

In channels the bot answers mentions, then follows the thread. Sending files needs the `files:write` scope (up to 100 MB), reactions need `reactions:write`, and approval buttons need Interactivity turned on for the app. Slack has no typing indicator for bots: with progress drafts off, a turn that runs past 60 seconds posts one "still working" notice.

## Discord, Signal and Matrix

```yaml
channels:
  discord:
    type: discord
    token_env: DISCORD_BOT_TOKEN
  signal:
    type: signal
    account: "+15551234567"           # the bot's number, registered or linked in signal-cli
    api_url: http://127.0.0.1:8080    # the default
  matrix:
    type: matrix
    homeserver: https://matrix.example.org
    token_env: MATRIX_ACCESS_TOKEN
    account: "@habakkuk:example.org"  # optional; checked against the token
    allow_from: ["@you:example.org"]  # also who may invite the bot into rooms
```

**Discord.** Create an application in the developer portal, add a bot, and switch on the **Message Content** intent (without it the gateway refuses the connection and `habakkuk channels` says so). Invite it with the `bot` scope and the View Channels, Send Messages, Send Messages in Threads, Read Message History and Attach Files permissions. A thread is its own conversation; once the bot has replied in a thread, later messages there count as addressed to it.

**Signal.** Habakkuk talks to [signal-cli](https://github.com/AsamK/signal-cli) running as a daemon in HTTP mode; signal-cli keeps the account's keys. Give the bot its own number, then either register it:

```bash
signal-cli -a +15551234567 register             # add --captcha ... if Signal asks for one
signal-cli -a +15551234567 verify 123-456       # the code from the SMS
```

or link signal-cli to an existing phone as a secondary device (`signal-cli link -n habakkuk` prints a `sgnl://` URI; turn it into a QR code and scan it under Settings > Linked devices). Then keep the daemon running, for example under systemd:

```bash
signal-cli -a +15551234567 daemon --http 127.0.0.1:8080
```

Keep it on loopback: the daemon has no authentication. `habakkuk doctor` checks that it answers. Add people to `allow_from` by number (`"+15557654321"`) or approve them with `habakkuk pair`.

**Matrix.** Make an account for the bot and get an access token, for example:

```bash
curl -s -X POST https://matrix.example.org/_matrix/client/v3/login -H 'Content-Type: application/json' \
  -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"habakkuk"},"password":"...","initial_device_display_name":"habakkuk"}'
```

Put the `access_token` in `MATRIX_ACCESS_TOKEN`, then invite the bot from an account in `allow_from` (or one approved with `habakkuk pair`); it joins on its own and ignores invitations from anyone else. A room with only you and the bot is a direct chat. End-to-end encryption is not supported yet: create the room without encryption. In an encrypted room the bot says once that it cannot read it, and stays quiet after that. The sync position is kept in `channels/matrix-<name>.json`, so a restart picks up where it stopped.

## Email, SMS, Teams and ntfy

Settings for these types live in a block named after the type:

```yaml
channels:
  mail:
    type: email
    allow_from: [you@example.com]
    email:
      address: habakkuk@example.com
      imap: imap.example.com:993             # IDLE when the server has it, else polling every minute
      smtp: smtp.example.com:587             # STARTTLS; 465 means implicit TLS
      password_env: HABAKKUK_MAIL_PASSWORD
  sms:
    type: sms
    sms: {account_sid: ACxxxxxxxx, auth_token_env: TWILIO_AUTH_TOKEN, from: "+15551234567"}
  teams:
    type: msteams
    msteams: {app_id: 00000000-0000-0000-0000-000000000000, app_password_env: TEAMS_APP_SECRET}
  push:
    type: ntfy
    allow_from: [ntfy]
    ntfy: {topic: habakkuk-out, subscribe: habakkuk-in, token_env: NTFY_TOKEN}
http:
  listen: 127.0.0.1:8484
  public_url: https://habakkuk.example.com     # a reverse proxy or tunnel to listen
```

Email replies keep the thread's `In-Reply-To` and `References` and a `Re:` subject; mail from lists, auto-responders and bounces is ignored, and so is mail whose receiving server recorded no passing DMARC or aligned DKIM result (`allow_unauthenticated: true` turns that off). Other email settings: `name` (the display name), `username` (default the address), `imap_tls` and `smtp_tls` (`auto`, `tls`, `starttls`, `none`), `folder` (default `INBOX`) and `poll` (default 1m). A session bound to an email channel starts tainted, since everything in an inbox is outside content.

SMS takes `messaging_service_sid` instead of `from` if you send through a Twilio messaging service. Teams takes `tenant_id` for single-tenant bots.

## Google Chat, iMessage, LINE, Mattermost and IRC

```yaml
channels:
  gchat:
    type: googlechat
    googlechat:
      audience: "123456789012"               # the project number or endpoint URL set as the Authentication Audience
      credentials_env: GOOGLE_CHAT_CREDENTIALS   # the service account's JSON key, or a path to it
  imessage:
    type: bluebubbles
    bluebubbles:
      url: http://mac.local:1234             # the BlueBubbles server
      password_env: BLUEBUBBLES_PASSWORD
      webhook_secret_env: BLUEBUBBLES_WEBHOOK_SECRET   # must match ?secret= on the webhook URL
      private_api: true                      # the server has the Private API: typing, replies
  line:
    type: line
    line: {channel_secret_env: LINE_CHANNEL_SECRET, access_token_env: LINE_ACCESS_TOKEN}
  mattermost:
    type: mattermost
    mattermost: {url: "https://chat.example.com", token_env: MATTERMOST_TOKEN}
  irc:
    type: irc
    irc:
      server: irc.libera.chat:6697           # TLS by default
      nick: habakkuk-bot
      password_env: IRC_PASSWORD             # SASL PLAIN
      channels: ["#habakkuk"]
```

The BlueBubbles webhook is `<public_url>/channels/<name>?secret=<webhook secret>`. In iMessage group chats a message is for Habakkuk when it names it (`mention`, default `Habakkuk`).

## WhatsApp Cloud API, Messenger and Instagram

In a Meta app, subscribe the webhook to `<public_url>/channels/<name>` with a verify token of your choice (put it in the `verify_token_env` variable) and the `messages` field. Requests are checked against `X-Hub-Signature-256` with the app secret. The WhatsApp Business Cloud API is described on the [WhatsApp page](whatsapp.md#business-cloud-api). Messenger and Instagram use a Page access token; replies outside their 24-hour window fail with the reason, since Habakkuk does not use message tags such as `human_agent`. Instagram fetches the files Habakkuk sends from one-hour URLs under the channel's path.

```yaml
channels:
  fbpage:
    type: messenger
    messenger: {access_token_env: FB_PAGE_TOKEN, app_secret_env: META_APP_SECRET, verify_token_env: META_VERIFY_TOKEN}
```

## Feishu, DingTalk, Zulip, Rocket.Chat, Nextcloud Talk, Twitch and WeCom

For Feishu (or Lark, with `domain: lark`), create a custom app with the bot capability, add the `im:message` permissions, and subscribe to `im.message.receive_v1` with the request URL `<public_url>/channels/<name>`; set an Encrypt Key and put it in `encrypt_key_env` so events arrive encrypted and signed. Senders are open ids (`ou_...`); with `reply_in_thread: true` each group mention is answered in a thread under it.

For DingTalk, add a robot to an internal app, choose HTTP mode and set the message receiving address to `<public_url>/channels/<name>`; senders are staff ids.

For Zulip, create a generic bot, put its API key in `api_key_env` and subscribe it to the streams it should read; a stream is the conversation `stream:<id>` and each topic a thread, direct messages are `dm:<user ids>`, and senders are user ids.

For Rocket.Chat, create a bot user, make it a personal access token (Profile, Personal Access Tokens) and set `user_id` and `token_env` from it; it follows every room it is in over the realtime API, rooms are conversations and threads threads.

For Nextcloud Talk, install a bot with `occ talk:bot:install --feature webhook --feature response --feature reaction Habakkuk <secret> <public_url>/channels/<name>` (a 40 to 128 character secret, also in `secret_env`) and enable it in the rooms it should read; Talk does not tell bots which rooms are one-to-one, so a message is for Habakkuk when it says `@Habakkuk` (`mention`), or set `groups: all`. Bots cannot send files.

For Twitch, make an account for the bot and a user access token for it with the `chat:read` and `chat:edit` scopes, and list the channels to join; chat is public, so only messages naming the bot (`@habakkuk_bot ...` or `habakkuk_bot: ...`) reach the agent, senders are numeric user ids, and replies are paced under Twitch's rate limit.

For WeCom, set the application's API receiving URL to `<public_url>/channels/<name>` with a Token and EncodingAESKey, and add the server's public IP to the app's trusted IPs; senders are member user ids.

```yaml
channels:
  feishu:
    type: feishu
    feishu: {app_id: cli_a1b2c3d4e5, app_secret_env: FEISHU_APP_SECRET, verification_token_env: FEISHU_VERIFICATION_TOKEN, encrypt_key_env: FEISHU_ENCRYPT_KEY}
  dingtalk:
    type: dingtalk
    dingtalk: {client_id: dingabcdef123456, client_secret_env: DINGTALK_CLIENT_SECRET}
  wecom:
    type: wecom
    wecom: {corp_id: ww0123456789abcdef, agent_id: 1000002, secret_env: WECOM_SECRET, token_env: WECOM_TOKEN, aes_key_env: WECOM_AES_KEY}
  zulip:
    type: zulip
    zulip: {url: "https://example.zulipchat.com", email: habakkuk-bot@example.zulipchat.com, api_key_env: ZULIP_API_KEY}
  rocketchat:
    type: rocketchat
    rocketchat: {url: "https://chat.example.com", user_id: aobEdbYhXfu5hkeqG, token_env: ROCKETCHAT_TOKEN}
  talk:
    type: nextcloudtalk
    groups: all
    nextcloudtalk: {url: "https://cloud.example.com", secret_env: TALK_BOT_SECRET}
  twitch:
    type: twitch
    allow_from: ["*"]                       # chat is public; or list user ids
    twitch: {login: habakkuk_bot, token_env: TWITCH_TOKEN, channels: [mystream]}
```
