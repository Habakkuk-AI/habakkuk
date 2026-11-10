---
title: Meetings
description: "Have an agent join Google Meet, Zoom and Microsoft Teams calls as a named guest, read the live captions, and send you notes with decisions and action items."
order: 5
---

Use this page when you want an agent to sit in a video call and take notes for you. Give an agent the `meeting` toolset and it can attend Google Meet, Zoom (web client) and Microsoft Teams calls as a named guest.

To send it to a call, ask: "join https://meet.google.com/abc-defg-hij and note the decisions". You can also schedule it. For example, "every weekday at 9:55, join my standup at <link>" becomes a schedule whose prompt calls `meeting_join`.

```yaml
agents:
  habakkuk:
    toolsets: [core, memory, skills, schedule, meeting]
meeting:                          # all optional
  name: Habakkuk (notetaker)      # display name in the call
  announce: true                  # post "<name> has joined to take notes." in the meeting chat
  max_duration: 3h                # longest stay, and the default limit
  lobby_timeout: 15m              # give up when nobody lets Habakkuk in
  alone_timeout: 10m              # leave after being the only one in the call this long
  headless: true
  summary_model: anthropic/claude-sonnet-5-5  # default: the agent's own model
```

Meetings need Chrome or Chromium, the same as the [browser](browser.md) toolset. In a container, that means the image built with `--target browser`.

## How it works

`meeting_join(url, name?, duration_limit?, notes_prompt?)` returns straight away. The attendance itself runs under `habakkuk serve` as a durable job. Here's what happens:

1. Habakkuk opens its own Chrome with a fresh profile. The `path`, `allow_private` and `blocked_urls` settings from the browser section apply to it.
2. It enters its name and turns the microphone and camera off. Both are fake devices anyway, sending silence and a black picture.
3. It joins the call. If it has to ask to join, it asks and waits in the lobby.
4. Once it's in, it turns on the platform's live captions. It reads them from the page into a transcript.
5. It leaves when the call ends, the time limit passes, everyone else leaves, or you say `meeting_leave`. Then it sends notes to the chat that asked, with a summary, decisions and action items.

Use `meeting_status` and `meeting_transcript` to see how it's going. There are terminal commands too:

```bash
habakkuk meetings                    # list
habakkuk meetings show [-transcript] ID
habakkuk meetings leave ID
```

If Habakkuk stops in the middle of a meeting, it joins again when it restarts and adds to the same transcript. The notes are still written and sent only once.

## Limits

- Habakkuk accepts meeting links from those three platforms only.
- Habakkuk joins as a guest. A meeting that requires a signed-in account or blocks guests refuses it.
- Habakkuk reads the page in English (`--lang=en-US`).
- Habakkuk doesn't speak in meetings and doesn't record audio. The transcript is only as good as the platform's captions, and the host may have turned those off.

> [!IMPORTANT]
> Let the people in the call know that a note-taker is there. `announce: true` is the default, and it posts a line in the meeting chat when Habakkuk joins.

For the details, see [Architecture: meetings](../reference/architecture.md#meetings).
