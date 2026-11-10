---
title: WhatsApp
description: "The three ways to reach Habakkuk on WhatsApp, compared: a WhatsApp agent from Settings > Agents, a linked device on your number, and the Business Cloud API."
order: 2
---

There are three ways to put Habakkuk on WhatsApp. This page compares them and shows you how to set up each one. They're separate channel types, and you can use them side by side.

| | WhatsApp agent (`whatsappagent`) | Linked device (`whatsapp`) | Business Cloud API (`whatsappcloud`) |
|---|---|---|---|
| What it is | An agent you create in WhatsApp, Settings > Agents | Habakkuk as a linked device of a phone number | A business phone number on Meta's hosted API |
| Who can write to it | Only the person who created the agent, while the platform is in beta | Anyone who messages that number, subject to `allow_from` and pairing | Customers who message the business number |
| Groups | No | Yes (mention or reply) | No |
| Public URL needed | No | No | Yes (webhook) |
| Account needed | A WhatsApp account | A WhatsApp number (its own is best) | A Meta app, a WhatsApp Business Account and a system user token |
| Voice replies | As an audio file | As voice notes | As voice notes |
| In `hk setup` | Yes (recommended) | Yes | No |

**Pick the WhatsApp agent** for a personal assistant. It takes a minute, and it gets its own chat with its own name and photo. **Pick the linked device** when other people need to reach the agent, or when you want it in groups. **Pick the Cloud API** for a business number that customers write to.

## WhatsApp agents

A WhatsApp agent is a chat you create inside WhatsApp. Habakkuk long-polls `api.whatsapp.com/agent/v1` with the agent's API key. That means you don't need a public URL, a linked device or a business account.

### Set it up

1. In WhatsApp, open **Settings > Agents** and create an agent. Give it a name and a photo.
2. Open the agent's chat and send it one message, such as "hi". That's how Habakkuk learns your id.
3. Still in the chat, open **Chat info > API key** and copy the key.
4. Run `hk setup` and pick **WhatsApp agent**. Or add the channel by hand:

```yaml
channels:
  wagent:
    type: whatsappagent
    token_env: WHATSAPP_AGENT_KEY     # the key goes in .env
```

5. Run `hk serve`.

### Access

While the platform is in beta, an agent can only talk to the person who created it. WhatsApp refuses everyone else. Habakkuk always treats that sender as the owner, even when you set `allow_from: ["*"]`. Schedules and `hk send` reach you at `<name>:creator`, for example `wagent:creator`.

> [!NOTE]
> These chats aren't end-to-end encrypted. Meta relays them.

### What works

- The agent receives text, images, video, voice notes, documents and stickers. It sends text, images, video, audio and documents.
- You get a typing indicator and read receipts.
- Audio goes out as a file, so it doesn't show as a voice-note bubble. The platform has no buttons, edits or reactions, so you answer approvals with **yes** or **no**.
- Habakkuk keeps the point where polling stopped in `channels/whatsappagent-<name>.json`, so you don't miss anything across a restart. The very first time it starts, Habakkuk answers only messages from the last ten minutes.

### Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `another program is polling with this agent's API key; only one may` | Only one program may poll with a key at a time. Stop the other `hk serve` (or other tool) using it. |
| `the API key is not valid; it changes when it is regenerated or WhatsApp is reinstalled` | Copy the key again from Chat info > API key into `.env`, then restart `serve`. |
| `the API key variable is empty` | The variable `token_env` names is not set in the environment or `.env`. |
| Schedules to `<name>:creator` fail | Send the agent one message first so Habakkuk learns your id. |

## Linked device

Habakkuk links to a phone number the same way WhatsApp Web does. It's best to use a second number, because Habakkuk doesn't answer messages you send from the linked number itself.

### Set it up

```yaml
channels:
  whatsapp:
    type: whatsapp
    groups: mention                             # off | mention | all
    allow_from: ["15551234567"]                 # or approve people with hk pair
```

Next, link the device:

```bash
hk whatsapp link                                # prints a QR code
hk whatsapp link -phone +15551234567            # prints a code to type on the phone instead
```

- **QR code.** Without `-phone`, the command prints a QR code in the terminal. It also saves the code as `$HABAKKUK_HOME/whatsapp-qr.png`. Scan it from WhatsApp > Settings > Linked devices > Link a device.
- **Pairing code.** With `-phone`, the command prints a code. On the phone, choose Link a device, then "Link with phone number instead". Enter the code there.

If you have several `whatsapp` channels, pass `-account NAME`. Linking gives up after five minutes. When that happens, run the command again. `hk setup` can also run the link for you.

### Access

`allow_from` takes phone numbers or LIDs. Anyone else who writes gets a pairing code, which you approve with `hk pair`. In groups, the bot answers when you mention it or reply to it. That's the `groups: mention` setting.

WhatsApp now identifies many senders by a LID in place of their phone number. A LID is a private id such as `98765432101234@lid`. Habakkuk checks `allow_from` against both. It looks up the phone number behind a LID, and the LID behind a phone number, in the linked device's store. That means either form works:

```yaml
    allow_from: ["+1 202 555 0142", "98765432101234"]
```

You can write a phone number with a `+`, spaces or dashes. You can write a LID with or without `@lid`. To see the ids Habakkuk sees for you, send `/whoami` in the chat.

### Sending to a number

`hk send`, a schedule's `-to` target and an agent's `home:` all accept a phone number, with or without the `+`:

```bash
hk send -to whatsapp:+12025550142 "The report is ready"
hk send -to whatsapp:98765432101234@lid "Same person, by LID"
```

Habakkuk turns a bare number into `<digits>@s.whatsapp.net`. It uses a full id as it is, such as `<digits>@lid` or a group's `...@g.us`. Some targets can never work, such as one with an unknown server after the `@`. Those fail at once, so Habakkuk doesn't retry them.

### What works

- Text, images, files and voice notes come in. Images up to 16 MB, documents up to 100 MB and voice notes go out.
- You see a typing indicator while a turn runs.
- Voice replies are sent as push-to-talk voice notes.

### Credentials

Habakkuk stores the device credentials in `channels/whatsapp-<account>.db`. It makes every change to them as a SQLite transaction, so a crash or a full disk can't leave the credentials file half-written.

Habakkuk also copies them to `backups/whatsapp-<account>-<time>.db`. It makes a copy when it connects, and once a day. The copy on connect happens at most once an hour, however often the connection drops. Habakkuk keeps the five newest copies, plus the newest copy from each of the last seven days. It never deletes the only good copy. Only you can read the copies (mode 0600, in a 0700 folder). That holds even when the process runs with a loose umask, for example under systemd without `UMask=`. `hk backup -env` includes the credentials too.

> [!WARNING]
> Anyone who has that database can act as the linked device, so keep any backup made with `-env` private.

## Business Cloud API

The `whatsappcloud` channel uses a business phone number on Meta's hosted API. You need a public URL for its webhook.

### Set it up

1. Open a Meta app that has the WhatsApp product. In WhatsApp Manager, note the phone number's **id**, which is a different value from the number itself. Then create a system user token.
2. Subscribe the webhook to `<public_url>/channels/<name>`. Use a verify token of your choice, and the `messages` field.
3. Put the token, the app secret and the verify token in `.env`. Then configure the channel:

```yaml
channels:
  wacloud:
    type: whatsappcloud
    allow_from: ["15551234567"]           # wa_id: the number without +
    whatsappcloud:
      phone_number_id: "123456789012345"
      access_token_env: WHATSAPP_TOKEN
      app_secret_env: META_APP_SECRET
      verify_token_env: META_VERIFY_TOKEN
      template: habakkuk_reply            # optional; approved, with one body variable
      template_language: en_US            # the default
      template_text: true
http:
  public_url: https://habakkuk.example.com
```

Habakkuk checks each request against `X-Hub-Signature-256`, using the app secret.

### The 24-hour window

WhatsApp lets a business write freely only within 24 hours of the customer's last message. Habakkuk records when each customer last wrote, in `channels/whatsappcloud-<name>.json`. Once that window has closed, it sends the approved `template` in place of a normal reply:

- With `template_text: true`, the reply fills the template's one `{{1}}` variable.
- Without it, the template goes out once, as an invitation to write back.
- With no template, the reply fails and gives the reason.

### What works

- The channel receives text, images, video, voice notes, documents and button replies.
- It sends text, images, video, audio, documents, reply buttons and lists. Approvals show as buttons.
- You get a typing indicator and read receipts.

Messenger and Instagram use the same Meta app setup. See [More platforms](more-platforms.md#messenger-and-instagram).
