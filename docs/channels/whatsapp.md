---
title: WhatsApp
description: "The three ways to reach Habakkuk on WhatsApp, compared: a WhatsApp agent from Settings > Agents, a linked device on your number, and the Business Cloud API."
order: 2
---

Habakkuk can be on WhatsApp in three ways. They are separate channel types and can be used side by side.

| | WhatsApp agent (`whatsappagent`) | Linked device (`whatsapp`) | Business Cloud API (`whatsappcloud`) |
|---|---|---|---|
| What it is | An agent you create in WhatsApp, Settings > Agents | Habakkuk as a linked device of a phone number | A business phone number on Meta's hosted API |
| Who can write to it | Only the person who created the agent, while the platform is in beta | Anyone who messages that number, subject to `allow_from` and pairing | Customers who message the business number |
| Groups | No | Yes (mention or reply) | No |
| Public URL needed | No | No | Yes (webhook) |
| Account needed | A WhatsApp account | A WhatsApp number (its own is best) | A Meta app, a WhatsApp Business Account and a system user token |
| Voice replies | As an audio file | As voice notes | As voice notes |
| In `habakkuk setup` | Yes (recommended) | Yes | No |

**Pick the WhatsApp agent** for a personal assistant: it takes a minute and gets its own chat with its own name and photo. **Pick the linked device** when other people should reach the agent, or in groups. **Pick the Cloud API** for a business number that customers write to.

## WhatsApp agents

A WhatsApp agent is a chat you create inside WhatsApp. Habakkuk long-polls `api.whatsapp.com/agent/v1` with the agent's API key, so it needs no public URL, no linked device and no business account.

### Set it up

1. In WhatsApp, open **Settings > Agents** and create an agent. Give it a name and photo.
2. Open its chat and send it one message ("hi"). Habakkuk then knows your id.
3. In the chat, open **Chat info > API key** and copy the key.
4. Run `habakkuk setup` and pick **WhatsApp agent**, or add it by hand:

```yaml
channels:
  wagent:
    type: whatsappagent
    token_env: WHATSAPP_AGENT_KEY     # the key goes in .env
```

5. Run `habakkuk serve`.

### Access

While the platform is in beta, an agent can only talk to the person who created it; WhatsApp refuses everyone else. Its sender is always treated as the owner, even with `allow_from: ["*"]`. Schedules and `habakkuk send` reach you at `<name>:creator`, for example `wagent:creator`.

> [!NOTE]
> These chats are not end-to-end encrypted: Meta relays them.

### What works

- Text, images, video, voice notes, documents and stickers in; text, images, video, audio and documents out.
- Typing indicator and read receipts.
- Audio goes out as a file, not a voice-note bubble. The platform offers no buttons, edits or reactions, so approvals are answered with **yes** or **no**.
- Where polling stopped is kept in `channels/whatsappagent-<name>.json`, so a restart misses nothing. On its very first start Habakkuk answers only messages from the last ten minutes.

### Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `another program is polling with this agent's API key; only one may` | Only one program may poll with a key at a time. Stop the other `habakkuk serve` (or other tool) using it. |
| `the API key is not valid; it changes when it is regenerated or WhatsApp is reinstalled` | Copy the key again from Chat info > API key into `.env`, then restart `serve`. |
| `the API key variable is empty` | The variable `token_env` names is not set in the environment or `.env`. |
| Schedules to `<name>:creator` fail | Send the agent one message first so Habakkuk learns your id. |

## Linked device

Habakkuk links to a number the way WhatsApp Web does. A second number is best: messages you send from the linked number itself are not answered.

### Set it up

```yaml
channels:
  whatsapp:
    type: whatsapp
    groups: mention                             # off | mention | all
    allow_from: ["15551234567"]                 # or approve people with habakkuk pair
```

Then link it:

```bash
habakkuk whatsapp link                          # prints a QR code
habakkuk whatsapp link -phone +15551234567      # prints a code to type on the phone instead
```

- **QR code.** Without `-phone`, the command prints a QR code in the terminal (and saves it as `$HABAKKUK_HOME/whatsapp-qr.png`). Scan it from WhatsApp > Settings > Linked devices > Link a device.
- **Pairing code.** With `-phone`, it prints a code. On the phone choose Link a device, then "Link with phone number instead", and enter the code.

With several `whatsapp` channels, pass `-account NAME`. Linking gives up after five minutes; run the command again. `habakkuk setup` can run the link for you.

### Access

`allow_from` takes phone numbers without the `+`. Anyone else who writes gets a pairing code for `habakkuk pair`. In groups the bot answers mentions and replies (`groups: mention`).

### What works

- Text, images, files and voice notes in; images up to 16 MB, documents up to 100 MB and voice notes out.
- Typing indicator while a turn runs.
- Voice replies go out as push-to-talk voice notes.

### Credentials

The device credentials live in `channels/whatsapp-<account>.db`. Every change to them is a SQLite transaction, so a crash or a full disk never leaves a half-written credentials file, and `backups/` keeps seven rotating copies. `habakkuk backup -env` includes them.

> [!WARNING]
> Anyone with that database can act as the linked device. Keep backups made with `-env` private.

## Business Cloud API

`whatsappcloud` uses a business phone number on Meta's hosted API. It needs a public URL for the webhook.

### Set it up

1. In a Meta app with the WhatsApp product, note the phone number's **id** from WhatsApp Manager (not the number itself), and create a system user token.
2. Subscribe the webhook to `<public_url>/channels/<name>` with a verify token of your choice and the `messages` field.
3. Put the token, the app secret and the verify token in `.env`, and configure the channel:

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

Requests are checked against `X-Hub-Signature-256` with the app secret.

### The 24-hour window

WhatsApp only lets a business write freely within 24 hours of the customer's last message. Habakkuk records when each customer last wrote in `channels/whatsappcloud-<name>.json`. Outside that window it sends the approved `template` instead:

- with `template_text: true`, the reply fills the template's one `{{1}}` variable;
- without it, the template goes out once as an invitation to write back;
- with no template, the reply fails with the reason.

### What works

- Text, images, video, voice notes, documents and button replies in.
- Text, images, video, audio, documents, reply buttons and lists out. Approvals show as buttons.
- Typing indicator and read receipts.

Messenger and Instagram use the same Meta app setup; see [More platforms](more-platforms.md#messenger-and-instagram).
