---
title: Channels
description: "Connect Habakkuk to chat apps: the supported channel types, who may talk to the bot, groups, webhooks and per-channel settings."
order: 1
---

A channel is one account on one chat platform. Channels live under `channels:` in `config.yaml`, keyed by an account name you choose, with a `type`. `habakkuk serve` runs them; `habakkuk channels` (or `/status` in a chat) shows their health.

## Supported types

| `type` | Connects by | Groups and threads | Files it sends | Config |
|---|---|---|---|---|
| `telegram` | long polling | groups (mention or reply), forum topics | photos up to 10 MB, documents up to 50 MB | `token_env` |
| `slack` | Socket Mode, one socket for every workspace | channels (mention), threads | up to 100 MB | `app_token_env`, `bot_tokens_env` |
| `whatsapp` | a linked device | groups (mention or reply) | images up to 16 MB, documents up to 100 MB | `habakkuk whatsapp link` |
| `discord` | Gateway websocket, with resume | servers (mention or reply), threads | up to 10 MB | `token_env` |
| `signal` | a signal-cli daemon | groups (mention, reply, or starting with the agent's name) | up to 100 MB | `account`, `api_url` |
| `matrix` | `/sync` long polling | rooms (mention or reply), threads; no end-to-end encryption | up to 50 MB | `homeserver`, `token_env`, `account` |
| `web` | HTTP and Server-Sent Events | none | none | the `http` section |

| Type | Connects by | Needs a public URL | In | Out | Typing | Groups |
|---|---|---|---|---|---|---|
| `telegram` | long polling | no | text, photos, files, voice | text, photos, files | yes | mention or reply |
| `slack` | Socket Mode | no | text, files | text, files | progress notice | mention, then the thread |
| `whatsapp` | linked device | no | text, images, files, voice | text, images, files | yes | mention or reply |
| `web` | Habakkuk's HTTP server | no | text | text | yes | none |
| `email` | IMAP (IDLE or polling) + SMTP | no | text, attachments | text, attachments | no | every thread is its own conversation |
| `sms` | Twilio webhook + REST | yes | SMS, MMS media | SMS, MMS (5 MB) | no | none |
| `msteams` | Bot Framework webhook | yes | text, files, pasted images | text; files in one-to-one chats (consent card), images in channels | yes | mention, threaded |
| `googlechat` | Chat app HTTP endpoint | yes | text, uploads | text, in the thread | no | mention |
| `bluebubbles` | BlueBubbles webhook + REST | yes | text, attachments | text, attachments | with the Private API | name mentioned |
| `line` | LINE webhook | yes | text, images, video, audio, files | text, JPEG and PNG images | one-to-one chats | mention |
| `mattermost` | WebSocket + REST | no | text, files | text, files | yes | mention, then the thread |
| `irc` | TLS, SASL PLAIN | no | text | text, split into lines | no | nick at the start or in the line |
| `ntfy` | JSON stream (optional) + publish | no | messages on the subscribe topic | messages, attachments | no | none |
| `whatsappagent` | WhatsApp agent platform long-poll (Settings > Agents) | no | text, images, video, voice, documents, stickers | text, images, video, audio, documents | yes, with read receipts | only the agent's creator |
| `whatsappcloud` | WhatsApp Business Cloud API webhook + Graph API | yes | text, images, video, voice, documents, button replies | text, images, video, audio, documents, reply buttons and lists; templates outside 24 hours | yes, with read receipts | none |
| `messenger` | Messenger Platform webhook + Graph API | yes | text, attachments, quick replies | text, attachments, quick replies | yes | none |
| `instagram` | Instagram messaging webhook + Graph API | yes | text, attachments, quick replies | text, images, video, audio, PDF (fetched from Habakkuk) | yes | none |
| `feishu` | Feishu or Lark event subscription + IM API | yes | text, rich text, images, files, audio, video | Markdown posts, images, files, voice, in threads | no | mention |
| `dingtalk` | DingTalk robot HTTP callback + robot API | yes | text, rich text, pictures, files, video, voice | Markdown, images, files | no | mention (DingTalk only sends those) |
| `wecom` | WeCom app callback + message API | yes | text, images, voice, video | text or markdown, images, video, files | no | none |
| `zulip` | event queue long-poll + REST | no | text, uploaded files | text, files as upload links | yes | mention; each topic is a thread |
| `rocketchat` | realtime API (WebSocket) + REST | no | text, files | text, files, in threads | no | mention |
| `nextcloudtalk` | Talk bot webhook + OCS bot API | yes | text (shared files named) | text, reactions | no | `@Habakkuk` in the text |
| `twitch` | Twitch chat over IRC (TLS) | no | chat messages | chat messages, threaded replies | no | `@login` or `login:` |

Setup for each platform:

- [WhatsApp](whatsapp.md): linked device, WhatsApp agents and the Business Cloud API.
- [Telegram, Slack, Discord and more](chat-platforms.md): every other type.
- [The dashboard](dashboard.md): the `web` chat and its API.

## Who may talk to the bot

Direct messages from someone not in `allow_from` get a pairing code for `habakkuk pair`: the owner approves the code (valid for an hour) and that sender may talk from then on. `allow_from: ["*"]` allows everyone; use it only with care (`habakkuk security audit` flags open channels). Approvals and clarify questions are answered only by allowed senders.

## Groups

In groups the bot answers only when addressed (`groups: mention`, the default); `groups: all` answers everything and `off` ignores groups. What counts as addressed depends on the platform: see the tables above.

## Webhook channels

Webhook channels (`sms`, `msteams`, `googlechat`, `bluebubbles`, `line`, `whatsappcloud`, `messenger`, `instagram`, `feishu`, `dingtalk`, `wecom`, `nextcloudtalk`) receive at `<public_url>/channels/<name>`; `habakkuk doctor` prints each URL. Every request is checked against the platform's signature or signed token, so these paths need no API token, but the platform must reach the server:

```yaml
http:
  listen: 127.0.0.1:8484
  public_url: https://habakkuk.example.com     # a reverse proxy or tunnel to listen
```

Adapter state that the platform cannot give back (where a mailbox was read up to, Teams service URLs, the last ntfy message) is kept in `$HABAKKUK_HOME/channels/<type>-<name>.json`.

## Settings every channel takes

| Setting | Meaning |
|---|---|
| `agent` | The default agent for conversations on this account |
| `routes` | A conversation (or Slack team) id mapped to another agent: `routes: {T0123456: ops}` |
| `allow_from` | Sender ids allowed to talk; `"*"` allows everyone |
| `groups` | `off`, `mention` (default) or `all` |
| `session_idle` | Start a fresh session after this much silence (default 24h; the web chat never) |
| `progress` | `draft` (default): a long turn posts one message and edits it with each tool step, on platforms that can edit; `off` turns it off |
| `reactions` | `true` reacts to the user's message when its turn starts and ends (Telegram, Slack, Discord) |
| `voice_replies` | `off` (default), `mirror` (answer voice with voice) or `always`; see [Voice](voice.md) |
| `voice_text` | With voice replies, also send the text (default `true`) |
| `language` | The language of Habakkuk's own messages on this channel |
| `api_url` | Override the platform's API base URL |

## How delivery works

- **Exactly once where it matters.** A turn's job ID is derived from the platform message ID, so a redelivered message never runs twice. A finished turn enqueues a delivery job with an ID derived from the turn, so a turn that reruns after a crash does not reply twice. A delivery records each chunk it sends, so a retry resumes at the first unsent chunk.
- **Attachments.** Photos, files and voice notes are downloaded (up to 25 MB), stored as blobs and copied to the agent's `workspace/inbox/` so tools can open them. Images and PDFs also go to the model directly. Voice notes are transcribed through the `transcription:` provider.
- **Sending files.** Agents send files with the `send_file` tool, within each platform's limits above.
- **Supervision.** Every adapter runs under a supervisor that restarts it with backoff and tears down a connection that stays silent past its limit.

[Architecture: channels](../reference/architecture.md#channels) has the details.

## Sending without a turn

`habakkuk send -to CHANNEL:CHAT[:THREAD] [-file PATH] TEXT` queues a message or file to a chat as a delivery job, without a turn; `habakkuk serve` sends it.
