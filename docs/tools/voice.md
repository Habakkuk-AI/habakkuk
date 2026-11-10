---
title: Voice
description: "Talk to Habakkuk out loud through Live mode in the dashboard, habakkuk talk on a laptop or Raspberry Pi, and voice notes with spoken replies in chats."
order: 4
---

You can talk to Habakkuk out loud. There are two ways to do it: Live mode in the dashboard, and `habakkuk talk` on the machine itself. Both hand the actual work to the agent, which has all its tools. Chat apps add a third way. You send a voice note, and a voice note can come back.

The [Speech](../models/speech.md) page explains how to configure the speech providers.

## Live mode in the dashboard

Add a `realtime` section and the chat at `/app/` gets a **Live** button. Your browser then talks to a realtime speech model over WebRTC, and you can interrupt it at any time. The browser connects with a short-lived secret that Habakkuk mints, so your API key never reaches the page.

When you ask for something real, the voice model calls Habakkuk. Habakkuk runs a normal turn and hands back the answer to be spoken. Everything the two of you say goes into the chat and into the conversation's session.

```yaml
realtime:
  provider: openai          # the only one for now
  model: gpt-realtime-2.1   # the default
  voice: marin              # the default; or cedar, alloy, ...
  key_env: OPENAI_API_KEY   # the default
```

There are four more settings: `base_url`, `transcription` (the model that writes your words into the log), `instructions` (added to the voice model's own instructions) and `ttl` (how long a minted secret lasts, default 10m).

> [!NOTE]
> The page needs a secure context to use the microphone. That means `localhost`, or HTTPS (for example `habakkuk expose tailscale`). The [desktop app](../interfaces/desktop-app.md) serves the page over loopback, so the microphone works there even when Habakkuk itself is on plain `http`.

If the agent wants approval for a step, the voice model asks you and passes on your yes or no. If a request runs past four minutes, you're told that the answer will appear in the chat.

## `habakkuk talk` on a laptop or a Raspberry Pi

This one is a local loop that runs through `ffmpeg`. It listens on the default microphone and notices when you stop talking. It transcribes what you said with your `transcription` provider and runs the turn. Then it speaks the reply with `tts`, played with `afplay`, `ffplay`, `paplay` or `aplay`. If you start talking while it speaks, it stops.

```bash
habakkuk talk                          # every utterance is a request
habakkuk talk -wake "hey habakkuk"     # only utterances that start with the phrase
habakkuk talk -agent ops -session ID   # another agent, or an existing session
```

```yaml
talk:                     # all optional
  wake: [hey habakkuk, ok habakkuk]
  follow_up: 30s          # after a reply, no wake phrase needed
  silence: 800ms          # a pause this long ends what you said
  threshold: 0.02         # the speech detector's level; it also adapts to the room
  input: pulse            # avfoundation (macOS), pulse or alsa
  device: default         # e.g. ":1" on macOS, "hw:1,0" for ALSA
  player: ffplay          # default: the first one found
  barge_in: true          # talk over a reply to stop it
```

Pass `-no-wake` to ignore the configured wake phrases for one run.

- Habakkuk hears the wake phrase by transcribing every utterance. In wake mode, that means each one costs a transcription call. An offline wake-word engine is future work.
- Use headphones if you can. There's no echo cancellation, so you have to talk a little louder to interrupt Habakkuk while it speaks.
- You answer approvals at the keyboard.

## Voice notes and spoken replies in chats

When you send the bot a voice note, Habakkuk transcribes it with the `transcription:` provider. From there it's an ordinary message.

Voice replies and the `speak` tool both use `tts:`. You turn voice replies on for each channel:

```yaml
channels:
  telegram:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN
    voice_replies: mirror   # off (default) | mirror (reply by voice to voice) | always
    voice_text: true        # also send the text (default)
```

- Voice notes go out as Ogg Opus. Audio in another format is converted with `ffmpeg` when that's installed. Otherwise it's sent as a plain audio file.
- Telegram, WhatsApp (linked device and Cloud API) and Feishu show them as voice notes. Other platforms get an audio file.
- Markdown is stripped from the spoken copy. A reply longer than `tts.max_chars` (default 2000) stays text only. If speaking fails, the text is sent in its place.

For Live mode, `habakkuk talk` and speech in depth, see [Architecture: media](../reference/architecture.md#media).
