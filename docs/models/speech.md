---
title: Speech
description: "Configure speech-to-text for voice notes and habakkuk talk, and text-to-speech for the speak tool and spoken replies."
order: 6
---

Speech has two sides, each with its own section in `config.yaml`. How you use them (voice notes, `habakkuk talk`, Live mode) is on the [Voice](../tools/voice.md) page.

## Speech-to-text

Voice notes from chats, dashboard recordings and `habakkuk talk` are transcribed through a provider of type `openai` with an `/audio/transcriptions` endpoint:

```yaml
transcription:
  provider: openai                # a configured provider with type: openai
  model: gpt-4o-mini-transcribe   # required
  prompt: "Names and words to spell right: Habakkuk, Toronto"   # optional
```

`provider` names an entry under `providers:`, so the key comes from there. A local server with the same endpoint works too.

Without a `transcription` section, the agent is told a voice note arrived that it cannot hear, and asks you to type it. If transcription fails, the agent gets the reason.

## Text-to-speech

The `speak` tool and spoken replies (`voice_replies` on a channel) use `tts:`:

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
> `edge` uses a free online voice service with no key. Like every provider, it is contacted only once you name it here.

## Live voice

The dashboard's Live mode uses a realtime speech model of its own, configured under `realtime:`. See [Voice: Live mode](../tools/voice.md#live-mode-in-the-dashboard).
