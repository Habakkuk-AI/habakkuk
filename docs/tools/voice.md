---
title: Voice
description: "Talk to Habakkuk out loud: Live mode in the dashboard, habakkuk talk on a laptop or Raspberry Pi, voice notes in chats, and spoken replies."
order: 4
---

Two ways to talk to Habakkuk out loud: Live mode in the dashboard, and `habakkuk talk` on the machine itself. Both hand the actual work to the agent with all its tools. Chat apps add a third: voice notes in, and voice notes out.

The speech providers are configured on the [Speech](../models/speech.md) page.

## Live mode in the dashboard

A `realtime` section adds a **Live** button to the chat at `/app/`. The browser talks to a realtime speech model over WebRTC (interrupt it any time), using a short-lived secret Habakkuk mints, so your API key never reaches the page. When you ask for something real, the voice model calls Habakkuk, which runs a normal turn and hands back the answer to be spoken. What both of you say lands in the chat and in the conversation's session.

```yaml
realtime:
  provider: openai          # the only one for now
  model: gpt-realtime-2.1   # the default
  voice: marin              # the default; or cedar, alloy, ...
  key_env: OPENAI_API_KEY   # the default
```

Other settings: `base_url`, `transcription` (the model that writes your words into the log), `instructions` (added to the voice model's own) and `ttl` (how long a minted secret lasts, default 10m).

> [!NOTE]
> The page needs a secure context for the microphone: `localhost`, or HTTPS (for example `habakkuk expose tailscale`). The [desktop app](../interfaces/desktop-app.md) serves it over loopback, so the microphone works there even when Habakkuk itself is plain `http`.

When the agent wants approval for a step, the voice model asks you and passes on your yes or no. A request that runs past four minutes tells you the answer will appear in the chat.

## `habakkuk talk` on a laptop or a Raspberry Pi

A local loop through `ffmpeg`. It listens on the default microphone, notices when you stop talking, transcribes with your `transcription` provider, runs the turn, and speaks the reply with `tts` (played with `afplay`, `ffplay`, `paplay` or `aplay`). Start talking while it speaks and it stops.

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

`-no-wake` ignores the configured wake phrases for one run.

- The wake phrase is heard by transcribing every utterance, so in wake mode each one costs a transcription call. An offline wake-word engine is future work.
- Use headphones if you can. There is no echo cancellation, so while Habakkuk speaks you have to talk a little louder to interrupt it.
- Approvals are answered at the keyboard.

## Voice notes and spoken replies in chats

Voice notes sent to the bot are transcribed with the `transcription:` provider and become ordinary messages.

The `speak` tool, and voice replies, use `tts:`. Turn voice replies on per channel:

```yaml
channels:
  telegram:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN
    voice_replies: mirror   # off (default) | mirror (reply by voice to voice) | always
    voice_text: true        # also send the text (default)
```

- Voice notes go out as Ogg Opus. Other audio is converted with `ffmpeg` when it is installed, or sent as a plain audio file.
- Telegram, WhatsApp (linked device and Cloud API) and Feishu show them as voice notes; other platforms get an audio file.
- The spoken copy is stripped of Markdown. A reply longer than `tts.max_chars` (default 2000) stays text only, and if speaking fails, the text goes instead.

[Architecture: media](../reference/architecture.md#media) describes Live mode, `habakkuk talk` and speech in depth.
