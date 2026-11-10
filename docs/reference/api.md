---
title: HTTP API
description: "The JSON API under /api/ that the dashboard uses, covering chat, sessions, board, schedules, memory, skills, usage, approvals, nodes and config."
order: 3
---

Every endpoint of Habakkuk's JSON API is listed here. Use it when you want to drive Habakkuk from your own code, the way the dashboard does.

When your config has an `http` section, `habakkuk serve` listens on the address in `listen` (default `127.0.0.1:8484`). It serves the chat page at `/`, the webhooks at `/hooks/<name>` and the OpenAI-compatible API under `/v1/` (see [Architecture](architecture.md#other-clients)). It also serves a control dashboard at `/app/` and the JSON API below. The dashboard is built on that API.

## Conventions

- **Auth.** Every `/api/` path needs an `Authorization: Bearer <token>` header. The token is the value of the variable that `http.token_env` names. If you don't set `token_env`, the server only listens on a loopback name and only answers requests addressed to one.
- **Changes are JSON.** Send `POST` and `PUT` with `Content-Type: application/json`. Where there's nothing to send, use an empty object. Anything else gets a `415`. A browser can't send that content type cross-site without a preflight, and the server never approves one. The server also refuses, with `403`, any request that a browser marks `Sec-Fetch-Site: cross-site` or `same-site`.
- **Errors** come back as `{"error": "..."}` with a status that fits. You get `400` for input the store rejected, and the message says why. You get `404` for an unknown agent, card, schedule, note, skill or session. You get `501` when this process has no such subsystem, or when speech or transcription isn't configured. Everything else is `500`, and the details go to the log so they stay out of the response.
- **Times** use RFC 3339 and are in UTC. Days in the usage report follow the `timezone` from your config.
- You refer to an agent by its name in the config. An unknown name gets a `404`.

## Dashboard

| Path | |
|---|---|
| `GET /app/` | The dashboard (static HTML, JavaScript and CSS, no data). It asks for the token once and keeps it in `localStorage`. |
| `GET /app/manifest.webmanifest`, `/app/sw.js`, `/app/icon.svg`, `/app/icon-{180,192,512}.png` | Install to home screen. The service worker caches only these static files, never an API response. |

To use the microphone, the page needs a secure context. That means `localhost`, or HTTPS in front of Habakkuk (a reverse proxy or tunnel).

## App

### `GET /api/app`

Returns what the page needs to start.

```json
{"account": "web", "agents": ["habakkuk", "ops"], "default": "habakkuk", "transcription": true, "speech": true, "realtime": true, "timezone": "America/Toronto"}
```

`transcription` tells you whether recordings can be sent (`transcription:` in config). `speech` tells you whether replies can be read aloud (`tts:`). `realtime` tells you whether Live mode is on (`realtime:`).

### `GET /api/overview`

```json
{
  "channels": [{"account": "tg", "type": "telegram", "state": "connected", "detail": "@habakkuk_bot", "last_error": "", "restarts": 0, "since": "...", "last_beat": "..."}],
  "queue": {"pending": 0, "running": 1},
  "running": [{"id": "turn_...", "kind": "turn", "agent": "habakkuk", "session": "ses_...", "status": "running", "attempts": 1, "created_at": "...", "updated_at": "..."}],
  "errors": [{"id": "...", "kind": "card", "agent": "habakkuk", "status": "failed", "error": "...", "...": "..."}],
  "approvals": 0,
  "open_cards": 3,
  "schedules": {"total": 4, "enabled": 3},
  "established": true
}
```

`channels` lists every configured account. An account's `state` is `never started` when `habakkuk serve` hasn't run it. `established` turns true once the install is in use. That means an agent has answered a message, or there's a schedule, or there's more than one messaging channel. The dashboard then hides its Get started panel. `errors` holds jobs from the last 7 days that failed for good, or that recorded an error on a retried attempt. You get the newest 20.

## Chat

### `POST /api/chat`

You can send a message two ways. As JSON, the body is `{conversation, text, agent?, id?}` (see ARCHITECTURE.md). As `multipart/form-data`, set the header `X-Habakkuk-Client` to any value and send these parts:

| Part | |
|---|---|
| `conversation`, `text`, `agent`, `id` | As in the JSON form. |
| `file` (repeatable, at most 10) | Attachments, like photos and documents on other channels: saved as blobs and to the agent's `workspace/inbox/`; images and PDFs also go to the model. |
| `audio` (optional) | A recording, transcribed with the configured `transcription` provider before the message is sent. The transcript (after any typed `text`) is the message, so a spoken "yes" answers an approval. |

All the files together can be up to 25 MB. The reply is `202 {"message": "...", "turn": "...", "transcript": "..."}`. `transcript` is there when you sent audio. The errors are `403` when `X-Habakkuk-Client` is missing, `501` for audio when transcription isn't configured, `502` when the transcription service failed, and `422` when it heard nothing.

### `GET /api/chat/conversations?limit=50`

Lists the web channel's conversations, newest first: `{"conversations": [{"conversation", "agent", "session", "title", "last_at"}]}`.

### `GET /api/chat/{conversation}/events?after=N`

Streams the conversation's messages as Server-Sent Events (see ARCHITECTURE.md).

### `GET /api/chat/{conversation}/live`

Streams what the conversation's current session is doing, as Server-Sent Events. The activity comes from the in-process events bus. If the conversation moves to a new session (after `/new` or switching agents), the stream follows it when the next turn starts. It sends two kinds of event:

- `session`: `{"session": "ses_...", "agent": "habakkuk"}`, sent when the stream starts following a session.
- `activity`: `{"kind", "tool", "id", "input", "text", "is_error"}`. Here `kind` is one of `text` (a piece of the reply as it streams), `thinking`, `tool_start` (with `tool`, `id` and `input`, which is the call's JSON input, cut at 2,000 characters), `tool_done` (with `id`, `is_error` and `text`, which is the result, cut at 4,000), `review` (an approval reviewer's decision), `retry`, `turn_done` or `turn_error`.

Activity is best effort and isn't replayed. The transcript is the record.

### `GET /api/chat/{conversation}/messages/{seq}/speech`

Reads one of the agent's replies aloud with the configured `tts` provider. You pick the reply by its event sequence number, and the Markdown is stripped before it's read. The body is audio: `audio/ogg` Opus or `audio/mpeg`, whichever the provider returns. You get `404` for a user message, `413` when the reply is over `tts.max_chars`, and `501` when there's no `tts`.

## Live mode

Live mode is real-time voice in the dashboard. It's configured under `realtime:` in config. The browser talks to the provider directly over WebRTC. These endpoints mint its credentials, bridge its one tool to Habakkuk and keep the transcript. Without `realtime:` they all return `501`, except the tool and transcript endpoints, which only need the web channel.

| Request | |
|---|---|
| `POST /api/realtime/session` `{"conversation", "agent"?}` | Mints a client secret for one call: `{"client_secret", "expires_at", "calls_url", "model", "voice", "tool": "ask_habakkuk"}`. Post the WebRTC offer SDP to `calls_url` with `Authorization: Bearer <client_secret>` and `Content-Type: application/sdp`; the answer SDP comes back. `503` when the key variable is not set, `502` when the provider refuses (its message is passed on). |
| `POST /api/realtime/tool` `{"conversation", "agent"?, "call_id"?, "request"}` | Runs an `ask_habakkuk` call as a turn of the conversation and returns `{"output": "..."}` for the voice model: the reply, a note that Habakkuk needs approval (answer with another call whose `request` is `yes` or `no`), or, after four minutes, that the answer will appear in the chat. The same `call_id` attaches to the first call's turn. The request shows in the chat as `(voice) <request>`. |
| `POST /api/realtime/transcript` `{"conversation", "lines": [{"role": "user" \| "assistant", "text"}]}` | Records finished transcript lines (at most 50 per request) in the chat's log as `(voice) <text>` and keeps them for the agent: `{"recorded": n}`. |
| `POST /api/realtime/end` `{"conversation", "agent"?}` | Writes the lines the agent has not seen into the conversation's session (starting one if the call never asked Habakkuk): `{"noted": n, "session"}`; `202` when a turn is running and the note will follow it. |

## Sessions

### `GET /api/sessions?agent=&limit=50&q=`

Without `q`, you get `{"sessions": [{"id", "agent", "title", "created_at", "updated_at"}]}`, most recently updated first. With `q`, you get a full-text search of user and assistant messages, across every agent or only one: `{"hits": [{"session", "agent", "title", "seq", "role", "at", "snippet"}]}`. Matched words are in `[brackets]`.

### `GET /api/sessions/{id}`

```json
{
  "session": {"id": "ses_...", "agent": "habakkuk", "title": "...", "created_at": "...", "updated_at": "..."},
  "messages": [{"seq": 0, "role": "user", "model": "", "blocks": [{"type": "text", "text": "..."}]}],
  "compaction": {"keep_from": 40, "summary": "..."}
}
```

A block is `text`, `tool_use` (with `tool`, `tool_use_id`, `input`), `tool_result` (with `tool_use_id`, `text`, `is_error`), `thinking`, `image` or `document` (with `name`, `media_type`). Any other block comes with its type only. Text over 20,000 characters is cut and marked `"cut": true`. `compaction` is there when older messages have been summarized.

### `GET /api/sessions/{id}/export`

Returns the transcript as a Markdown file (`Content-Disposition: attachment`), in the same form `/export` sends it.

## Board

Changes you make here are made as the user (`created_by: "user"`). The user can change any card.

| Request | Body | |
|---|---|---|
| `GET /api/board?status=open&assignee=&limit=200` | | `{"cards": [...]}`. `status` is `open` (todo, doing, blocked), `all`, or one status. |
| `GET /api/board/{id}` | | One card with `comments` and `events` (its history). |
| `POST /api/board` | `{"title", "body"?, "assignee"?, "priority"?, "parent"?, "depends_on"?}` | `201` with the card. |
| `POST /api/board/{id}/assign` | `{"assignee": "ops"}` | `""` unassigns. Reassigning a card in progress stops its run and puts it back to todo. |
| `POST /api/board/{id}/priority` | `{"priority": 5}` | |
| `POST /api/board/{id}/comment` | `{"text": "..."}` | |
| `POST /api/board/{id}/cancel` | `{}` | Stops a running attempt. |
| `POST /api/board/{id}/reopen` | `{}` | A blocked card goes back to todo for another attempt. |

A card looks like this: `{"id", "title", "body", "assignee", "created_by", "status", "priority", "result", "parent", "depends_on", "attempt", "session", "created_at", "updated_at", "comments": [{"author", "text", "at"}], "events": [{"author", "kind", "text", "at"}]}`. Every action sends the card back.

## Schedules

| Request | Body | |
|---|---|---|
| `GET /api/schedules?agent=` | | `{"schedules": [...]}`, next run first. |
| `POST /api/schedules` | `{"agent", "name", "when", "prompt", "deliver_to"?}` | `201`. `when` is cron (`0 8 * * 1-5`), `@every 2h`, `@daily` or `at 2026-12-01 09:00` in the config timezone. `deliver_to` is `ACCOUNT:CONVERSATION[:THREAD]` (for the web account everything after the colon is the conversation); without it results stay in the run's session. |
| `POST /api/schedules/{id}/pause` | `{}` | |
| `POST /api/schedules/{id}/resume` | `{}` | Resumes from now; missed slots are not replayed. |
| `POST /api/schedules/{id}/run` | `{}` | Queues one extra run now: `{"job": "sch_...", "schedule": {...}}`. |
| `DELETE /api/schedules/{id}` | | `204`. |

A schedule looks like this: `{"id", "agent", "name", "spec", "prompt", "command", "on_output", "deliver_to", "enabled", "next_run", "last_run", "last_status", "last_output", "fail_streak", "created_at"}`. The API only adds prompt schedules. Script schedules run shell commands unattended, so you add those with `habakkuk schedules add -run`. A body with `command` is rejected as an unknown field. You can still list, pause, run and delete script schedules here.

## Memory

| Request | Body | |
|---|---|---|
| `GET /api/memory/{agent}?q=&tags=a,b&limit=` | | Without `q` or `tags`: the notes, newest first, `{"memories": [...], "total": N}`. With them: ranked matches with a `snippet`. |
| `GET /api/memory/{agent}/{slug}` | | One note with its `body`. |
| `POST /api/memory/{agent}` | `{"title", "content", "tags"?}` | Creates the note whose slug derives from the title (`201`), or replaces it (`200`). |
| `PUT /api/memory/{agent}/{slug}` | `{"title", "content", "tags"?}` | Replaces a note. A new title means a new slug: the note is saved under it and the old file moves to the trash. `tags` omitted keeps them. |
| `DELETE /api/memory/{agent}/{slug}` | | Moves the note to `agents/<agent>/memory/.trash/` (`204`). |

A note looks like this: `{"slug", "title", "tags", "created_at", "updated_at", "body", "snippet"}`.

### Memory history

Habakkuk keeps every change to an agent's notes, `USER.md`, `IDENTITY.md` and own skills. It also keeps changes to `SOUL.md`, once Habakkuk has changed it. Changes are kept for `memory.history_days` (see [Memory history and undo](../agents/memory.md#memory-history-and-undo)).

| Request | Body | |
|---|---|---|
| `GET /api/memory-history/{agent}?file=&kind=&since=&limit=` | | Changes, newest first, `{"changes": [...], "pins": [{"kind", "name"}]}`. `file` is a slug, `USER.md`, `IDENTITY.md`, `SOUL.md` or a skill name; `kind` is `note`, `profile`, `identity`, `skill` or `soul`; `since` is `7d`, `12h` or a date. |
| `GET /api/memory-history/{agent}/{id}` | | One change with its unified `diff`. `id` may be shortened to four characters. |
| `POST /api/memory-history/{agent}/{id}/restore` | `{"version"?: "before" \| "after"}` | Puts back the content before the change (default: undo) or after it (redo). The restore is recorded as a change by the user: `{"restored": {...}, "unchanged": false}`, or `"unchanged": true` when the file already held that version. |

A change looks like this: `{"id", "agent", "kind", "name", "path", "op", "old_hash", "new_hash", "by", "tool", "session", "tainted", "note", "restores", "at"}`. `op` is `created`, `updated` or `deleted`. `by` is `agent`, `user`, `review`, `curator`, `retro`, `hand` or `system`. `tainted` is true when external content had entered the turn that made the change.

### Retros

These endpoints let you read an agent's [retrospectives](../agents/retros.md). They're read only. To approve or reject proposals, use `habakkuk retro apply`, or `/retro apply` in a chat.

| Request | Body | |
|---|---|---|
| `GET /api/retros/{agent}?limit=` | | Retros, newest first, `{"retros": [...]}`, without their evidence. |
| `GET /api/retros/{agent}/{id}` | | One retro with its `evidence`. `id` may be `latest`. |

A retro looks like this: `{"id", "agent", "from", "to", "on_demand", "plan", "evidence", "actions", "report", "delivered", "done", "summary", "created_at"}`. `plan` holds `summary`, `went_well`, `went_badly` (each `{"what", "cause", "evidence"}`), `follow_up` and `dropped` (what validation threw out). Each action looks like this: `{"n", "kind", "target", "description", "find", "change", "why", "expected", "risk", "evidence", "status", "result", "updated_at"}`. Its `status` is `proposed`, `applied`, `approved`, `rejected` or `failed`. `report` is the path of the Markdown report under `$HABAKKUK_HOME`.

## Skills

| Request | |
|---|---|
| `GET /api/skills/{agent}` | `{"skills": [{"name", "description", "shared"}]}`: the agent's own skills and the shared ones, an agent's own winning a name clash. |
| `GET /api/skills/{agent}/{name}` | `{"name", "description", "shared", "body", "files"}`: the SKILL.md instructions and the supporting files' paths. |

## Usage

### `GET /api/usage?days=30&agent=`

Reports the tokens and cost of every successful model call over the last `days` days (1 to 400), counting today.

```json
{
  "since": "2026-10-09", "timezone": "America/Toronto",
  "total": {"key": "total", "calls": 120, "input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0, "cache_write_tokens": 0, "cost": 4.12, "unpriced": 0},
  "days": [{"key": "2026-10-09", "...": "..."}],
  "by_model": [{"key": "anthropic/claude-sonnet-5-5", "...": "..."}],
  "by_agent": [{"key": "habakkuk", "...": "..."}]
}
```

`days` has one row per day, including the days where everything is zero. Cost is in dollars. It's worked out from `pricing:` in config and the built-in table. Calls through a subscription login cost 0. `unpriced` counts the calls whose model has no known price, and their cost isn't included.

### `GET /api/budget?agent=`

Shows what's been spent today and this month against the spending limits (`budget:` in config). It covers the whole install and each agent. Pass `agent` to keep only one. A `limit` of 0 (left out) means there's no limit. `paused` is set while a limit is used up, and it holds the message that turns end with.

```json
{
  "timezone": "America/Toronto", "day": "2026-11-09", "month": "2026-11", "unknown_price": "allow",
  "install": {"today": {"spent": 4.2, "limit": 5, "percent": 84, "resets": "2026-11-10T00:00:00-05:00"},
              "month": {"spent": 31.5, "limit": 100, "percent": 31.5, "resets": "2026-12-01T00:00:00-05:00"},
              "per_turn": 1.5},
  "agents": [{"agent": "habakkuk", "today": {"spent": 4.2, "resets": "..."}, "month": {"...": "..."},
              "paused": "Paused: the daily budget of $5.00 for this install is used up; ..."}]
}
```

## Approvals

These are the tool calls waiting for a yes or no in a chat. A call waits there under permission mode `ask`, under `workspace` for exec, or when a reviewer was unsure.

| Request | |
|---|---|
| `GET /api/approvals` | `{"approvals": [{"id", "to": {"account", "conversation", "thread"}, "session", "tool", "summary", "review", "since"}]}`, oldest first. |
| `POST /api/approvals/{id}` `{"approve": true}` | Allows (or with `false` denies) the call, exactly as answering in the chat would, and posts "Approved from the dashboard." there. `404` when it is no longer waiting (answered, timed out, or the turn stopped). |

## Nodes

These endpoints are there when the config has a `nodes` section. The devices themselves use `/nodes` (see [ARCHITECTURE.md](architecture.md#device-nodes)) instead of this API.

| Request | |
|---|---|
| `GET /api/nodes` | `{"nodes": [{"name", "platform", "caps", "paired_at", "last_seen", "online"}]}`, by name; a connected node's `caps` are what it offers now. |
| `POST /api/nodes/pair` `{}` | `{"code", "expires"}`: a one-time pairing code, as `habakkuk node pair` makes. |
| `DELETE /api/nodes/{name}` | Revokes the node (its secret stops working) and drops its connection. `404` when there is no such node. |

## Config

### `GET /api/config`

```json
{"home": "/home/me/.habakkuk", "config": {"providers": {...}, "agents": {...}}, "doctor": [{"name": "provider anthropic", "ok": true, "detail": "2/2 keys present"}]}
```

`config` is the configuration in effect, with defaults applied. It's read-only, and secrets are redacted like this:

- The value of every variable the config names is replaced with `[redacted]` wherever it appears. That covers `*_env` fields, MCP `env` maps and `${VAR}` in MCP headers.
- A string field is replaced whole when its name reads like a secret (token, password, key, ...) and it doesn't name a variable. So is an MCP header value that isn't a variable reference.
- Passwords in URLs and secret-looking query parameters are replaced. So are strings shaped like well-known API keys, bearer tokens, JWTs and private keys.

Under each channel, `progress` and `approvals_detail` show the values in effect now. They may be newer than the ones `serve` started with.

`doctor` runs the `habakkuk doctor` checks that the running process can see. It covers provider keys and logins, the integrity of `control.db` and `transcripts.db`, and channel, HTTP and MCP credentials. Credentials are named by their variable, so their values never appear. It also covers MCP servers that failed to connect.

### `GET /api/channels`

Returns what each chat channel shows while the agent works and in approval requests. It lists messaging channels only, so the web chat isn't here.

```json
{
  "channels": [{"account": "slack", "type": "slack", "progress": "plain", "approvals_detail": "summary", "live_progress": true}],
  "progress": ["plain", "detailed", "off"],
  "approvals_detail": ["summary", "full"],
  "writable": true
}
```

`live_progress` tells you whether the platform can edit a message. Without it, a long turn shows a typing indicator or one notice, whatever `progress` says. At the top level, `progress` and `approvals_detail` list the values each setting takes. `writable` is false in a process that can't save the config.

### `PUT /api/channels/{account}`

```json
{"progress": "detailed", "approvals_detail": "full"}
```

Changes one or both settings for a channel. The answer is the channel's entry, in the same shape as in the list. The change is written to `config.yaml` in place. Comments are kept, and the previous file is saved as `config.yaml.bak`. The change applies from the channel's next message, so you don't need to restart. You get `400` for a value that isn't one of those listed, or for a body with neither setting. You get `404` for an account that isn't a messaging channel, and `501` when the process can't save. Like every change in this API, it needs the token and a JSON body.
