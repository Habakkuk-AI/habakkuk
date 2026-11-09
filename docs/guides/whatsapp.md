---
title: WhatsApp
description: "The three ways to reach Habakkuk on WhatsApp: a linked device on your number, a WhatsApp agent from Settings > Agents, and the Business Cloud API."
order: 2
---

Habakkuk can be on WhatsApp in three ways. They are separate channel types and can be used side by side.

| | Linked device (`whatsapp`) | WhatsApp agent (`whatsappagent`) | Business Cloud API (`whatsappcloud`) |
|---|---|---|---|
| What it is | Habakkuk as a linked device of a phone number | An agent you create in WhatsApp, Settings > Agents | A business phone number on Meta's hosted API |
| Who can write to it | Anyone who messages that number, subject to `allow_from` and pairing | Only the person who created the agent, while the platform is in beta | Customers who message the business number |
| Groups | Yes (mention or reply) | No | No |
| Public URL needed | No | No | Yes (webhook) |
| Account needed | A WhatsApp number (its own is best) | A WhatsApp account | A Meta app, a WhatsApp Business Account and a system user token |

## Linked device

Habakkuk links to a number the way WhatsApp Web does, and keeps the device credentials in `channels/whatsapp-<account>.db`. Every change to them is a SQLite transaction, so a crash or a full disk never leaves a half-written credentials file, and `backups/` keeps seven rotating copies.

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

Without `-phone`, the command prints a QR code in the terminal (and saves it as `$HABAKKUK_HOME/whatsapp-qr.png`): scan it from WhatsApp > Settings > Linked devices > Link a device. With `-phone`, it prints a pairing code: on the phone choose Link a device, then "Link with phone number instead", and enter the code. With several `whatsapp` channels, pass `-account NAME`. Linking gives up after five minutes.

The linked device shows typing while a turn runs, receives text, images, files and voice notes, sends images up to 16 MB and documents up to 100 MB, and sends voice replies as push-to-talk voice notes.

## WhatsApp agents

In WhatsApp, open Settings > Agents, create an agent, then open its chat and copy Chat info > API key into the `token_env` variable. Habakkuk long-polls `api.whatsapp.com/agent/v1` with that key, so it needs no public URL, no linked device and no business account, and WhatsApp shows Habakkuk as its own chat with the name and photo you gave it. While the platform is in beta an agent can only talk to the person who created it, and these chats are not end-to-end encrypted (Meta relays them). Send the agent one message first: Habakkuk then knows your id, and schedules and `habakkuk send` can reach you at `<name>:creator`. Where polling stopped is kept in `channels/whatsappagent-<name>.json`, so a restart misses nothing; on its very first start Habakkuk answers only messages from the last ten minutes. Only one program may poll with a key at a time; a second one makes the first stop with an error. Audio goes out as a file, not a voice-note bubble, and buttons, edits and reactions are not offered by the platform.

```yaml
channels:
  wagent:
    type: whatsappagent
    token_env: WHATSAPP_AGENT_KEY
```

## Business Cloud API

`whatsappcloud` is a separate type from `whatsapp`, which links Habakkuk as a device of a personal number. In a Meta app, subscribe the webhook to `<public_url>/channels/<name>` with a verify token of your choice (put it in the `verify_token_env` variable) and the `messages` field. Requests are checked against `X-Hub-Signature-256` with the app secret. `whatsappcloud` takes the phone number's id from WhatsApp Manager and a system user token.

WhatsApp only lets a business write freely within 24 hours of the customer's last message; Habakkuk records when each customer last wrote in `channels/whatsappcloud-<name>.json`, and outside that window sends the approved `template` instead (with `template_text: true` the reply fills the template's one `{{1}}` variable; without, the template goes out once as an invitation to write back), or fails the reply with the reason.

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
      template_text: true
http:
  public_url: https://habakkuk.example.com
```

Messenger and Instagram use the same Meta app setup; see [Telegram, Slack, Discord and more](chat-platforms.md#whatsapp-cloud-api-messenger-and-instagram).
