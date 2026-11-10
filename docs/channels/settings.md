---
title: Access, groups and delivery
nav_title: Shared settings
description: "Who may talk to the bot, pairing codes, group rules, webhook channels, the settings every channel takes, how delivery works, and what each platform supports."
order: 1
---

Some rules and settings apply to every channel type. They control who can talk to the bot, how it behaves in groups, and how messages get delivered. For setup, see each platform's own page.

## Who may talk to the bot

`allow_from` lists the sender ids that are allowed to talk. The kind of id depends on the platform. It's a numeric Telegram user id, a phone number without `+` for WhatsApp, or an email address for email. Each channel's page tells you which to use.

When anyone else sends a direct message, they get a pairing code:

> Hi! I don't recognise you yet. To get access, ask my owner to run: `habakkuk pair 4KX9QT`. The code works for one hour.

```bash
habakkuk pair 4KX9QT
```

Once you run that, the sender can talk from then on. To see who you are to the bot and how you got access, send `/whoami` in a chat.

> [!WARNING]
> `allow_from: ["*"]` lets everyone in. Those senders are **guests**. A guest never gets the risky tools: commands, writes outside the workspace, the browser on private addresses, and computer use. A guest also can't answer your approvals, and can't use `/remember`, `/forget` or `/agent`. `habakkuk security` flags open channels. See [Security](../deploy/security.md).

Only allowed senders can answer approvals and clarifying questions. A `whatsappagent` channel only ever hears from the person who created the agent, so Habakkuk always treats its sender as the owner.

## Groups

In a group, the bot answers only when someone addresses it. The `groups` setting controls this for each channel:

| `groups` | The bot answers |
|---|---|
| `mention` (default) | Messages addressed to it: a mention, a reply to it, or the platform's equivalent |
| `all` | Every message in groups it is in |
| `off` | Nothing in groups; direct messages only |

What counts as addressing the bot depends on the platform. The table [below](#what-each-platform-supports) lists it for each one.

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

A platform that needs more settings takes them in a block named after its type (`email:`, `sms:`, `msteams:` and so on).

## What a chat shows while the agent works

When a turn runs for more than 8 seconds, the chat shows that the agent is working. The `progress` setting controls how:

| `progress` | The chat shows |
|---|---|
| `plain` (default) | One short line in everyday words, edited as the work moves on and removed when the reply arrives |
| `detailed` | One message with a line per tool step: the tool's name and its command or path. `draft` is the old name for this and still works |
| `off` | Nothing but the typing indicator, or one "still working" notice |

You won't see a tool name, a command, a path, an id or JSON in the plain line. It only tells you what kind of work is going on:

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

After about 20 seconds, the line also says how long the turn has been running. For example, `Working on it… (40s)`, and later `(2 min)`. Habakkuk translates these lines into every [supported language](../get-started/configuration.md).

Editing a message only works on Telegram and Slack. On every other platform, `plain` and `detailed` show the same thing `off` does. That's the typing indicator where the platform has one, or one "still working" notice after a minute. The terminal, the terminal UI and the dashboard always show each step.

## Approval requests in plain words

When the agent needs your approval in a chat, it says what it wants to do in everyday words. It also tells you why it's asking and how to answer:

```text
I'd like to add a note to decisions.md in your Corner Bakery workspace.
I'm asking because this conversation included a web page or email, and I check with you before acting on those.

Reply yes or no. Reply details to see the exact command.
```

- **The first line** is built from the tool call itself, with no model involved, so the same call always reads the same way. It names a shell command only when the command is one simple step, such as adding to a file, deleting one, `git push` or contacting a website. Anything longer reads as "I'd like to run a command on your computer". That way the line never says less than the command does.
- **The second line** appears when there's a reason beyond the permission mode. The reasons are: the turn read outside content, the request came from a chat, the tool always asks, or the [reviewer](../agents/approvals.md#approvals) was unsure.
- **Reply `details`** to see the exact command or call. Secrets are removed and long calls are cut short. The approval keeps waiting for your answer.
- **`approvals_detail: full`** shows the exact call in every request, in a code block. The local web chat does this by default.

On platforms with buttons, you get Approve and Deny as well. For who may answer and what happens across a restart, see [Permissions and approvals](../agents/approvals.md#answering-approvals).

## Changing these without a restart

You can change `progress` and `approvals_detail` while `habakkuk serve` is running. The change takes effect from the next message, and you don't need to restart. Each of the ways below edits `config.yaml` in place. Your comments stay, and the previous file is kept as `config.yaml.bak`.

- **Dashboard.** On the Config page, each chat channel has two controls: "While working, show" and "Approval requests show".
- **In the chat.** Send `/progress plain|detailed|off` or `/approvals summary|full` to change the channel you're writing in. With no argument, each one shows the current setting. Only the owner can use them.
- **Terminal.** Run `habakkuk setup channel NAME -progress plain -approvals-detail summary`. `NAME` is the channel's name under `channels:`. When you have only one channel of that type, you can use the type.
- **By hand.** Edit `config.yaml`. `serve` picks up the change within a few seconds.

Every other channel setting still needs a restart. The log tells you so.

## Webhook channels

The webhook channels are `sms`, `msteams`, `googlechat`, `bluebubbles`, `line`, `whatsappcloud`, `messenger`, `instagram`, `feishu`, `dingtalk`, `wecom` and `nextcloudtalk`. Each one receives at `<public_url>/channels/<name>`, where `<name>` is the account name. Run `habakkuk doctor` to print each URL. Habakkuk checks every request against the platform's signature or signed token, so these paths don't need an API token. The platform does have to reach the server:

```yaml
http:
  listen: 127.0.0.1:8484
  public_url: https://habakkuk.example.com     # a reverse proxy or tunnel to listen
```

To put a proxy or tunnel in front, see [Remote access](../deploy/remote-access.md).

Some adapter state is something the platform can't give back: where a mailbox was read up to, Teams service URLs, the last ntfy message. Habakkuk keeps that state in `$HABAKKUK_HOME/channels/<type>-<name>.json`.

## How delivery works

- **Exactly once where it matters.** Habakkuk derives a turn's job ID from the platform message ID, so a message that's delivered again doesn't run a second time. When a turn finishes, it queues a delivery job with an ID derived from the turn. So if the turn reruns after a crash, it doesn't reply twice. A delivery records each chunk it sends, so a retry resumes at the first chunk that wasn't sent.
- **Attachments.** Habakkuk downloads photos, files and voice notes (up to 25 MB). It stores them as blobs and copies them to the agent's `workspace/inbox/`, so tools can open them. Images and PDFs also go straight to the model. Voice notes are transcribed through the [`transcription:` provider](../models/speech.md#speech-to-text).
- **Sending files.** An agent sends files with the `send_file` tool. Each platform's limits apply.
- **Replies arrive whole.** Habakkuk doesn't stream text into the chat as it's written. During a long turn you see progress: one short line that changes as the work moves on, a typing indicator, or a "still working" notice. See [above](#what-a-chat-shows-while-the-agent-works).
- **Supervision.** Every adapter runs under a supervisor. The supervisor restarts the adapter with backoff. It also tears down a connection that stays silent past its limit.

For the details, see [Architecture: channels](../reference/architecture.md#channels).

## Replies and quotes

When you reply to a specific message, the agent sees the quoted text first, then what you typed:

```text
[Replying to you: "Tests pass. Merge it?"]
yes
```

Here "you" means the agent's own message. For anyone else's message, it names the person. When the platform doesn't name them, it says "an earlier message". The quote is cut to 500 characters. A photo, file or voice note without text shows as `(photo)`, `(file NAME)` or `(voice note)`.

| Type | Quoted text |
|---|---|
| `whatsapp` | yes, for text and media replies |
| `telegram` | yes, or just the part you picked; not the first message of a forum topic |
| `signal` | yes |
| `discord` | yes |
| others | not yet: those platforms send only the id of the quoted message, and Habakkuk does not look it up |

Some messages reach a chat from outside its own conversation, such as a scheduled task's result. [Sessions](../agents/sessions.md#messages-from-elsewhere-in-the-same-chat) covers those.

## Sending without a turn

```bash
habakkuk send -to CHANNEL:CHAT[:THREAD] [-file PATH] TEXT
```

`habakkuk send` queues a message or file for a chat as a delivery job. It doesn't run a turn. `habakkuk serve` then sends it. Schedules, board cards and an agent's home chat use the same `CHANNEL:CHAT[:THREAD]` form. The agent that answers the chat sees what was sent when the next message arrives there.

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

On every platform, you can answer an approval by typing **yes** or **no**. Platforms with buttons show Approve and Deny too.
