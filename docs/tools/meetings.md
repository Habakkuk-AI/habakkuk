---
title: Meetings
description: "Let an agent attend Google Meet, Zoom and Microsoft Teams calls as a named guest, read the live captions, and send notes with decisions and action items."
order: 5
---

Agents that list the toolset `meeting` can attend Google Meet, Zoom (web client) and Microsoft Teams calls as a named guest and take notes. Ask "join https://meet.google.com/abc-defg-hij and note the decisions", or schedule it: "every weekday at 9:55, join my standup at <link>" becomes a schedule whose prompt calls `meeting_join`.

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

Meetings need Chrome or Chromium, as for the [browser](browser.md) toolset (in a container, the image built with `--target browser`).

## How it works

`meeting_join(url, name?, duration_limit?, notes_prompt?)` returns at once; the attendance runs under `habakkuk serve` as a durable job.

1. Habakkuk opens a Chrome of its own with a fresh profile. The browser section's `path`, `allow_private` and `blocked_urls` apply.
2. It enters its name and turns the microphone and camera off (they are fake devices sending silence and a black picture anyway).
3. It joins, or asks to join and waits in the lobby.
4. Once in, it turns on the platform's live captions and reads them from the page into a transcript.
5. When the call ends, the time limit passes, everyone else leaves, or you say `meeting_leave`, it leaves and sends notes with a summary, decisions and action items to the chat that asked.

`meeting_status` and `meeting_transcript` show progress. From the terminal:

```bash
habakkuk meetings                    # list
habakkuk meetings show [-transcript] ID
habakkuk meetings leave ID
```

A Habakkuk that stops mid-meeting joins again when it restarts and appends to the same transcript; notes are written and sent once.

## Limits

- Only meeting links of the three platforms are accepted.
- Habakkuk joins as a guest, so meetings that require a signed-in account or block guests refuse it.
- The page is read in English (`--lang=en-US`).
- Habakkuk does not speak in meetings and records no audio. The transcript is only as good as the platform's captions, which the host may have turned off.

> [!IMPORTANT]
> Tell the people in the call that a note-taker is present. `announce: true` (the default) posts a line in the meeting chat when it joins.

[Architecture: meetings](../reference/architecture.md#meetings) has the details.
