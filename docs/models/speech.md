---
title: Speech
description: "Set up speech-to-text for voice notes and hk talk, and text-to-speech for the speak tool and spoken replies."
order: 6
---

Speech has two sides: speech-to-text and text-to-speech. Each has its own section in `config.yaml`, and this page shows you how to set up both. For how you use them (voice notes, `hk talk`, Live mode), see the [Voice](../tools/voice.md) page.

## Speech-to-text

Habakkuk transcribes voice notes from chats, dashboard recordings and `hk talk`. It does this through a provider of type `openai` that has an `/audio/transcriptions` endpoint:

```yaml
transcription:
  provider: openai                # a configured provider with type: openai
  model: gpt-4o-mini-transcribe   # required
  prompt: "Names and words to spell right: Habakkuk, Toronto"   # optional
```

`provider` is the name of an entry under `providers:`, so that's where the key comes from. A local server that has the same endpoint works too.

If there's no `transcription` section, the agent is told that a voice note arrived and that it can't hear it. It then asks you to type the message. If transcription fails, the agent is told the reason.

## Text-to-speech

Two things use the `tts:` section: the `speak` tool, and spoken replies (`voice_replies` on a channel).

```yaml
tts:
  provider: openai          # openai, elevenlabs, or edge (free, no key)
  key_env: OPENAI_API_KEY
  model: gpt-4o-mini-tts
  voice: alloy
```

| Setting | Meaning |
|---|---|
| `provider` | `openai`, `elevenlabs` or `edge` |
| `key_env` | The variable holding the API key (`edge` needs none) |
| `base_url` | Override the provider's endpoint |
| `model` | `openai`: default `gpt-4o-mini-tts`; `elevenlabs`: default `eleven_multilingual_v2` |
| `voice` | `openai`: default `alloy`; `elevenlabs`: a voice id; `edge`: default `en-US-AriaNeural` |
| `format` | `opus` (default; voice notes) or `mp3`; `edge` always produces mp3 |
| `instructions` | Steer delivery (tone, pace) on models that take them |
| `max_chars` | The longest text spoken (default 2000); longer replies stay text only |

> [!NOTE]
> `edge` uses a free online voice service that needs no key. As with every provider, Habakkuk contacts it only once you name it here.

## Live voice

Live mode in the dashboard has a realtime speech model of its own. You configure it under `realtime:`. See [Voice: Live mode](../tools/voice.md#live-mode-in-the-dashboard).
