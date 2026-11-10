---
title: Access, groups and delivery
nav_title: Shared settings
description: "Who may talk to the bot, pairing codes, group rules, webhook channels, the settings every channel takes, how delivery works, and what each platform supports."
order: 1
---

These rules and settings apply to every channel type. Each platform's own page covers its setup.

## Who may talk to the bot

`allow_from` lists the sender ids allowed to talk. Ids depend on the platform: a numeric Telegram user id, a phone number without `+` for WhatsApp, an email address for email. Each channel page says which.

Direct messages from anyone else get a pairing code:

> Hi! I don't recognise you yet. To get access, ask my owner to run: `habakkuk pair 4KX9QT`. The code works for one hour.

```bash
habakkuk pair 4KX9QT
```

The sender may talk from then on. `/whoami` in a chat shows who you are to the bot and how you got access.

> [!WARNING]
> `allow_from: ["*"]` lets everyone in. Such senders are **guests**: they never get risky tools (commands, writes outside the workspace, the browser on private addresses, computer use), cannot answer your approvals, and cannot use `/remember`, `/forget` or `/agent`. `habakkuk security` flags open channels. See [Security](../deploy/security.md).

Approvals and clarifying questions are answered only by allowed senders. A `whatsappagent` channel only ever hears from the person who created the agent, so its sender is always treated as the owner.

## Groups

In groups the bot answers only when addressed. `groups` sets the rule per channel:

| `groups` | The bot answers |
|---|---|
| `mention` (default) | Messages addressed to it: a mention, a reply to it, or the platform's equivalent |
| `all` | Every message in groups it is in |
| `off` | Nothing in groups; direct messages only |

What counts as addressed depends on the platform; see the table [below](#what-each-platform-supports).

## Settings every channel takes

| Setting | Meaning |
|---|---|
| `type` | The platform |
| `agent` | The default agent for conversations on this account |
| `routes` | A conversation (or Slack team) id mapped to another agent: `routes: {T0123456: ops}` |
| `allow_from` | Sender ids allowed to talk; `"*"` allows everyone |
| `groups` | `off`, `mention` (default) or `all` |
| `session_idle` | Start a fresh session after this much silence (default `24h`; the web chat never; `0` never) |
| `progress` | What a chat shows while the agent works: `plain` (default), `detailed` or `off`. See [below](#what-a-chat-shows-while-the-agent-works) |
| `approvals_detail` | What an approval request shows: `summary` (default) or `full`. See [below](#approval-requests-in-plain-words) |
| `reactions` | `true` reacts to the user's message when its turn starts and ends (Telegram, Slack, Discord, Nextcloud Talk) |
| `voice_replies` | `off` (default), `mirror` (answer voice with voice) or `always`; see [Voice](../tools/voice.md#voice-notes-and-spoken-replies-in-chats) |
| `voice_text` | With voice replies, also send the text (default `true`) |
| `language` | The language of Habakkuk's own messages on this channel |
| `api_url` | Override the platform's API base URL |

Platforms that need more settings take them in a block named after the type (`email:`, `sms:`, `msteams:` and so on).

## What a chat shows while the agent works

A turn that runs past 8 seconds shows that it is working. `progress` sets how:

| `progress` | The chat shows |
|---|---|
| `plain` (default) | One short line in everyday words, edited as the work moves on and removed when the reply arrives |
| `detailed` | One message with a line per tool step: the tool's name and its command or path. `draft` is the old name for this and still works |
| `off` | Nothing but the typing indicator, or one "still working" notice |

The plain line never shows a tool name, a command, a path, an id or JSON. It says what kind of work is going on:

| The agent is | The line says |
|---|---|
| reading or changing files, or running a command | Looking through your files |
| searching or reading the web, or using the browser | Checking the web |
| reading or saving memory | Checking my notes |
| handing work to another agent or the board | Handing this to Ada (the agent's own name) |
| setting up a schedule or reminder | Setting that up |
| using an MCP server | Checking Google Calendar (the server's name) |
| making an image, a video or speech | Making that |
| anything else | Working on it |

After about 20 seconds the line also says how long the turn has run: `Working on it… (40s)`, then `(2 min)`. These lines are translated into every [supported language](../get-started/configuration.md).

Only Telegram and Slack can edit a message. On every other platform `plain` and `detailed` show what `off` shows: the typing indicator where there is one, or one "still working" notice after a minute. The terminal, the terminal UI and the dashboard always show each step.

## Approval requests in plain words

When the agent needs your approval in a chat, it says what it wants to do in everyday words, why it is asking, and how to answer:

```text
I'd like to add a note to decisions.md in your Corner Bakery workspace.
I'm asking because this conversation included a web page or email, and I check with you before acting on those.

Reply yes or no. Reply details to see the exact command.
```

- **The first line** comes from the tool call itself, with no model involved, so the same call always reads the same. A shell command is named only when it is one simple step, such as adding to a file, deleting one, `git push` or contacting a website. Anything longer reads as "I'd like to run a command on your computer", so the line never says less than the command does.
- **The second line** is there when the reason is more than the permission mode: the turn read outside content, the request came from a chat, the tool always asks, or the [reviewer](../agents/approvals.md#approvals) was unsure.
- **Reply `details`** to see the exact command or call, with secrets removed and long calls cut short. The approval keeps waiting.
- **`approvals_detail: full`** puts the exact call in every request, in a code block. The local web chat does this by default.

Platforms with buttons show Approve and Deny as well. [Permissions and approvals](../agents/approvals.md#answering-approvals) covers who may answer and what happens across a restart.

## Changing these without a restart

`progress` and `approvals_detail` can be changed while `habakkuk serve` runs. The change applies from the next message, with no restart. Every way below edits `config.yaml` in place: comments stay, and the previous file is kept as `config.yaml.bak`.

- **Dashboard.** The Config page has two controls for each chat channel: "While working, show" and "Approval requests show".
- **In the chat.** `/progress plain|detailed|off` and `/approvals summary|full` change the channel you are writing in. With no argument they show the current setting. Only the owner can use them.
- **Terminal.** `habakkuk setup channel NAME -progress plain -approvals-detail summary`. `NAME` is the channel's name under `channels:`, or its type when you have only one of that type.
- **By hand.** Edit `config.yaml`; `serve` notices within a few seconds.

Every other channel setting still needs a restart, and the log says so.

## Webhook channels

Webhook channels (`sms`, `msteams`, `googlechat`, `bluebubbles`, `line`, `whatsappcloud`, `messenger`, `instagram`, `feishu`, `dingtalk`, `wecom`, `nextcloudtalk`) receive at `<public_url>/channels/<name>`, where `<name>` is the account name. `habakkuk doctor` prints each URL. Every request is checked against the platform's signature or signed token, so these paths need no API token, but the platform must reach the server:

```yaml
http:
  listen: 127.0.0.1:8484
  public_url: https://habakkuk.example.com     # a reverse proxy or tunnel to listen
```

[Remote access](../deploy/remote-access.md) shows how to put a proxy or tunnel in front.

Adapter state that the platform cannot give back (where a mailbox was read up to, Teams service URLs, the last ntfy message) is kept in `$HABAKKUK_HOME/channels/<type>-<name>.json`.

## How delivery works

- **Exactly once where it matters.** A turn's job ID is derived from the platform message ID, so a redelivered message never runs twice. A finished turn enqueues a delivery job with an ID derived from the turn, so a turn that reruns after a crash does not reply twice. A delivery records each chunk it sends, so a retry resumes at the first unsent chunk.
- **Attachments.** Photos, files and voice notes are downloaded (up to 25 MB), stored as blobs and copied to the agent's `workspace/inbox/` so tools can open them. Images and PDFs also go to the model directly. Voice notes are transcribed through the [`transcription:` provider](../models/speech.md#speech-to-text).
- **Sending files.** Agents send files with the `send_file` tool, within each platform's limits.
- **Replies arrive whole.** Text is not streamed into the chat as it is written. A long turn shows progress instead: one short line that changes as the work moves on, a typing indicator, or a "still working" notice. See [above](#what-a-chat-shows-while-the-agent-works).
- **Supervision.** Every adapter runs under a supervisor that restarts it with backoff and tears down a connection that stays silent past its limit.

[Architecture: channels](../reference/architecture.md#channels) has the details.

## Replies and quotes

When you reply to a specific message, the agent sees the quoted text before what you typed:

```text
[Replying to you: "Tests pass. Merge it?"]
yes
```

"you" means the agent's own message; otherwise it names the person, or says "an earlier message" when the platform does not. The quote is cut to 500 characters, and a photo, file or voice note without text shows as `(photo)`, `(file NAME)` or `(voice note)`.

| Type | Quoted text |
|---|---|
| `whatsapp` | yes, for text and media replies |
| `telegram` | yes, or just the part you picked; not the first message of a forum topic |
| `signal` | yes |
| `discord` | yes |
| others | not yet: those platforms send only the id of the quoted message, and Habakkuk does not look it up |

Messages a chat got from outside its own conversation, such as a scheduled task's result, are covered in [Sessions](../agents/sessions.md#messages-from-elsewhere-in-the-same-chat).

## Sending without a turn

```bash
habakkuk send -to CHANNEL:CHAT[:THREAD] [-file PATH] TEXT
```

`habakkuk send` queues a message or file to a chat as a delivery job, without running a turn; `habakkuk serve` sends it. Schedules, board cards and an agent's home chat use the same `CHANNEL:CHAT[:THREAD]` form. The agent that answers the chat sees what was sent on the next message there.

## What each platform supports

| Type | In | Out | Typing | Buttons | Groups |
|---|---|---|---|---|---|
| `telegram` | text, photos, files, voice | text, photos (10 MB), documents (50 MB), voice notes, polls | yes | yes | mention or reply; forum topics |
| `slack` | text, files | text, files (100 MB) | progress notice | yes | mention, then the thread |
| `discord` | text, files | text, files (10 MB), polls | yes | yes | mention or reply; threads |
| `whatsapp` | text, images, files, voice | text, images (16 MB), documents (100 MB), voice notes | yes | no | mention or reply |
| `whatsappagent` | text, images, video, voice, documents, stickers | text, images, video, audio, documents | yes, with read receipts | no | none: only the agent's creator |
| `whatsappcloud` | text, images, video, voice, documents, button replies | text, images, video, audio, documents, reply buttons and lists; templates outside 24 hours | yes, with read receipts | yes | none |
| `signal` | text, attachments | text, attachments (100 MB) | yes | no | mention, reply, or starting with the agent's name |
| `matrix` | text, files | text, files (50 MB) | yes | no | mention or reply; threads; no end-to-end encryption |
| `email` | text, attachments | text, attachments | no | no | every thread is its own conversation |
| `sms` | SMS, MMS media | SMS, MMS (5 MB) | no | no | none |
| `msteams` | text, files, pasted images | text; files in one-to-one chats (consent card), images in channels | yes | no | mention, threaded |
| `googlechat` | text, uploads | text, in the thread | no | no | mention |
| `bluebubbles` | text, attachments | text, attachments | with the Private API | no | name mentioned |
| `web` | text | text | yes | in the dashboard | none |
| `line` | text, images, video, audio, files | text, JPEG and PNG images | one-to-one chats | no | mention |
| `mattermost` | text, files | text, files | yes | no | mention, then the thread |
| `irc` | text | text, split into lines | no | no | nick at the start or in the line |
| `ntfy` | messages on the subscribe topic | messages, attachments | no | no | none |
| `messenger` | text, attachments, quick replies | text, attachments | yes | quick replies | none |
| `instagram` | text, attachments, quick replies | text, images, video, audio, PDF | yes | quick replies | none |
| `feishu` | text, rich text, images, files, audio, video | Markdown posts, images, files, voice, in threads | no | no | mention |
| `dingtalk` | text, rich text, pictures, files, video, voice | Markdown, images, files | no | no | mention |
| `wecom` | text, images, voice, video | text or markdown, images, video, files | no | no | none |
| `zulip` | text, uploaded files | text, files as upload links | yes | no | mention; each topic is a thread |
| `rocketchat` | text, files | text, files, in threads | no | no | mention |
| `nextcloudtalk` | text (shared files named) | text, reactions | no | no | `@Habakkuk` in the text |
| `twitch` | chat messages | chat messages, threaded replies | no | no | `@login` or `login:` |

Every platform takes a typed **yes** or **no** for an approval. Platforms with buttons show Approve and Deny too.
