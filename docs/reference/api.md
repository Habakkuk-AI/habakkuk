---
title: HTTP API
description: "The JSON API under /api/ that the dashboard is built on: chat, sessions, board, schedules, memory, skills, usage, approvals, nodes and config."
order: 3
---

`habakkuk serve` with an `http` section listens on `listen` (default `127.0.0.1:8484`). Besides the chat page at `/`, the webhooks at `/hooks/<name>` and the OpenAI-compatible API under `/v1/` (see [Architecture](architecture.md#other-clients)), it serves a control dashboard at `/app/` and the JSON API below, which the dashboard is built on.

## Conventions

- **Auth.** Every `/api/` path needs `Authorization: Bearer <token>`, the value of the variable `http.token_env` names. Without `token_env` the server only listens on, and only answers requests addressed to, a loopback name.
- **Changes are JSON.** `POST` and `PUT` take `Content-Type: application/json` (an empty object where nothing is needed); anything else is `415`. Browsers cannot send that cross-site without a preflight, which the server never approves. Requests a browser marks `Sec-Fetch-Site: cross-site` or `same-site` are refused with `403`.
- **Errors** are `{"error": "..."}` with a fitting status: `400` for input the store rejected (the message says why), `404` for an unknown agent, card, schedule, note, skill or session, `501` when this process has no such subsystem (or speech or transcription is not configured), `500` otherwise (details go to the log, not the response).
- **Times** are RFC 3339 in UTC. Days in the usage report use the config `timezone`.
- Agents are referred to by their config name; an unknown name is `404`.

## Dashboard

| Path | |
|---|---|
| `GET /app/` | The dashboard (static HTML, JavaScript and CSS, no data). It asks for the token once and keeps it in `localStorage`. |
| `GET /app/manifest.webmanifest`, `/app/sw.js`, `/app/icon.svg`, `/app/icon-{180,192,512}.png` | Install to home screen. The service worker caches only these static files, never an API response. |

The page needs a secure context for the microphone: `localhost`, or HTTPS in front of Habakkuk (a reverse proxy or tunnel).

## App

### `GET /api/app`

What the page needs to start.

```json
{"account": "web", "agents": ["habakkuk", "ops"], "default": "habakkuk", "transcription": true, "speech": true, "realtime": true, "timezone": "America/Toronto"}
```

`transcription` says whether recordings can be sent (`transcription:` in config); `speech` whether replies can be read aloud (`tts:`); `realtime` whether Live mode is on (`realtime:`).

### `GET /api/overview`

```json
{
  "channels": [{"account": "tg", "type": "telegram", "state": "connected", "detail": "@habakkuk_bot", "last_error": "", "restarts": 0, "since": "...", "last_beat": "..."}],
  "queue": {"pending": 0, "running": 1},
  "running": [{"id": "turn_...", "kind": "turn", "agent": "habakkuk", "session": "ses_...", "status": "running", "attempts": 1, "created_at": "...", "updated_at": "..."}],
  "errors": [{"id": "...", "kind": "card", "agent": "habakkuk", "status": "failed", "error": "...", "...": "..."}],
  "approvals": 0,
  "open_cards": 3,
  "schedules": {"total": 4, "enabled": 3}
}
```

`channels` lists every configured account (`state` is `never started` when `habakkuk serve` has not run it). `errors` are jobs that failed for good, or recorded an error on a retried attempt, in the last 7 days (newest 20).

## Chat

### `POST /api/chat`

As JSON, `{conversation, text, agent?, id?}` (see ARCHITECTURE.md). As `multipart/form-data`, with the header `X-Habakkuk-Client` set to any value:

| Part | |
|---|---|
| `conversation`, `text`, `agent`, `id` | As in the JSON form. |
| `file` (repeatable, at most 10) | Attachments, like photos and documents on other channels: saved as blobs and to the agent's `workspace/inbox/`; images and PDFs also go to the model. |
| `audio` (optional) | A recording, transcribed with the configured `transcription` provider before the message is sent. The transcript (after any typed `text`) is the message, so a spoken "yes" answers an approval. |

All files together may be 25 MB. The reply is `202 {"message": "...", "turn": "...", "transcript": "..."}`; `transcript` is present when there was audio. Errors: `403` without `X-Habakkuk-Client`, `501` for audio when transcription is not configured, `502` when the transcription service failed, `422` when it heard nothing.

### `GET /api/chat/conversations?limit=50`

The web channel's conversations, newest first: `{"conversations": [{"conversation", "agent", "session", "title", "last_at"}]}`.

### `GET /api/chat/{conversation}/events?after=N`

Server-Sent Events of the conversation's messages (see ARCHITECTURE.md).

### `GET /api/chat/{conversation}/live`

Server-Sent Events of the activity of the conversation's current session, from the in-process events bus. It follows the conversation to a new session (after `/new` or switching agents) when the next turn starts. Events:

- `session`: `{"session": "ses_...", "agent": "habakkuk"}` when the stream starts following a session.
- `activity`: `{"kind", "tool", "id", "input", "text", "is_error"}` where `kind` is `text` (a piece of the reply as it streams), `thinking`, `tool_start` (`tool`, `id`, `input`: the call's JSON input, cut at 2,000 characters), `tool_done` (`id`, `text`: the result, cut at 4,000, `is_error`), `review` (an approval reviewer's decision), `retry`, `turn_done` or `turn_error`.

Activity is best effort and not replayed; the transcript is the record.

### `GET /api/chat/{conversation}/messages/{seq}/speech`

One of the agent's replies (by its event sequence number) read aloud with the configured `tts` provider, Markdown stripped. The body is audio (`audio/ogg` Opus or `audio/mpeg`, as the provider returns). `404` for a user message, `413` over `tts.max_chars`, `501` without `tts`.

## Live mode

Real-time voice in the dashboard (`realtime:` in config). The browser talks to the provider directly over WebRTC; these endpoints mint its credentials, bridge its one tool to Habakkuk and keep the transcript. All return `501` without `realtime:`, except the tool and transcript endpoints, which only need the web channel.

| Request | |
|---|---|
| `POST /api/realtime/session` `{"conversation", "agent"?}` | Mints a client secret for one call: `{"client_secret", "expires_at", "calls_url", "model", "voice", "tool": "ask_habakkuk"}`. Post the WebRTC offer SDP to `calls_url` with `Authorization: Bearer <client_secret>` and `Content-Type: application/sdp`; the answer SDP comes back. `503` when the key variable is not set, `502` when the provider refuses (its message is passed on). |
| `POST /api/realtime/tool` `{"conversation", "agent"?, "call_id"?, "request"}` | Runs an `ask_habakkuk` call as a turn of the conversation and returns `{"output": "..."}` for the voice model: the reply, a note that Habakkuk needs approval (answer with another call whose `request` is `yes` or `no`), or, after four minutes, that the answer will appear in the chat. The same `call_id` attaches to the first call's turn. The request shows in the chat as `(voice) <request>`. |
| `POST /api/realtime/transcript` `{"conversation", "lines": [{"role": "user" \| "assistant", "text"}]}` | Records finished transcript lines (at most 50 per request) in the chat's log as `(voice) <text>` and keeps them for the agent: `{"recorded": n}`. |
| `POST /api/realtime/end` `{"conversation", "agent"?}` | Writes the lines the agent has not seen into the conversation's session (starting one if the call never asked Habakkuk): `{"noted": n, "session"}`; `202` when a turn is running and the note will follow it. |

## Sessions

### `GET /api/sessions?agent=&limit=50&q=`

Without `q`: `{"sessions": [{"id", "agent", "title", "created_at", "updated_at"}]}`, most recently updated first. With `q`: a full-text search of user and assistant messages across agents (or one), `{"hits": [{"session", "agent", "title", "seq", "role", "at", "snippet"}]}`; matched words are in `[brackets]`.

### `GET /api/sessions/{id}`

```json
{
  "session": {"id": "ses_...", "agent": "habakkuk", "title": "...", "created_at": "...", "updated_at": "..."},
  "messages": [{"seq": 0, "role": "user", "model": "", "blocks": [{"type": "text", "text": "..."}]}],
  "compaction": {"keep_from": 40, "summary": "..."}
}
```

Blocks are `text`, `tool_use` (`tool`, `tool_use_id`, `input`), `tool_result` (`tool_use_id`, `text`, `is_error`), `thinking`, `image` and `document` (`name`, `media_type`), and others by type only. Text over 20,000 characters is cut and marked `"cut": true`. `compaction` is present when older messages were summarized.

### `GET /api/sessions/{id}/export`

The transcript as a Markdown file (`Content-Disposition: attachment`), as `/export` sends it.

## Board

Changes are made as the user (`created_by: "user"`), who may change any card.

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

A card is `{"id", "title", "body", "assignee", "created_by", "status", "priority", "result", "parent", "depends_on", "attempt", "session", "created_at", "updated_at", "comments": [{"author", "text", "at"}], "events": [{"author", "kind", "text", "at"}]}`. Every action returns the card.

## Schedules

| Request | Body | |
|---|---|---|
| `GET /api/schedules?agent=` | | `{"schedules": [...]}`, next run first. |
| `POST /api/schedules` | `{"agent", "name", "when", "prompt", "deliver_to"?}` | `201`. `when` is cron (`0 8 * * 1-5`), `@every 2h`, `@daily` or `at 2026-12-01 09:00` in the config timezone. `deliver_to` is `ACCOUNT:CONVERSATION[:THREAD]` (for the web account everything after the colon is the conversation); without it results stay in the run's session. |
| `POST /api/schedules/{id}/pause` | `{}` | |
| `POST /api/schedules/{id}/resume` | `{}` | Resumes from now; missed slots are not replayed. |
| `POST /api/schedules/{id}/run` | `{}` | Queues one extra run now: `{"job": "sch_...", "schedule": {...}}`. |
| `DELETE /api/schedules/{id}` | | `204`. |

A schedule is `{"id", "agent", "name", "spec", "prompt", "command", "on_output", "deliver_to", "enabled", "next_run", "last_run", "last_status", "last_output", "fail_streak", "created_at"}`. The API only adds prompt schedules: script schedules run shell commands unattended, so they are added with `habakkuk schedules add -run` (a body with `command` is rejected as an unknown field). The others can be listed, paused, run and deleted here.

## Memory

| Request | Body | |
|---|---|---|
| `GET /api/memory/{agent}?q=&tags=a,b&limit=` | | Without `q` or `tags`: the notes, newest first, `{"memories": [...], "total": N}`. With them: ranked matches with a `snippet`. |
| `GET /api/memory/{agent}/{slug}` | | One note with its `body`. |
| `POST /api/memory/{agent}` | `{"title", "content", "tags"?}` | Creates the note whose slug derives from the title (`201`), or replaces it (`200`). |
| `PUT /api/memory/{agent}/{slug}` | `{"title", "content", "tags"?}` | Replaces a note. A new title means a new slug: the note is saved under it and the old file moves to the trash. `tags` omitted keeps them. |
| `DELETE /api/memory/{agent}/{slug}` | | Moves the note to `agents/<agent>/memory/.trash/` (`204`). |

A note is `{"slug", "title", "tags", "created_at", "updated_at", "body", "snippet"}`.

### Memory history

Every change to an agent's notes, `USER.md`, `IDENTITY.md`, own skills and (once Habakkuk has changed it) `SOUL.md`, kept for `memory.history_days` (see [Memory history and undo](../agents/memory.md#memory-history-and-undo)).

| Request | Body | |
|---|---|---|
| `GET /api/memory-history/{agent}?file=&kind=&since=&limit=` | | Changes, newest first, `{"changes": [...], "pins": [{"kind", "name"}]}`. `file` is a slug, `USER.md`, `IDENTITY.md`, `SOUL.md` or a skill name; `kind` is `note`, `profile`, `identity`, `skill` or `soul`; `since` is `7d`, `12h` or a date. |
| `GET /api/memory-history/{agent}/{id}` | | One change with its unified `diff`. `id` may be shortened to four characters. |
| `POST /api/memory-history/{agent}/{id}/restore` | `{"version"?: "before" \| "after"}` | Puts back the content before the change (default: undo) or after it (redo). The restore is recorded as a change by the user: `{"restored": {...}, "unchanged": false}`, or `"unchanged": true` when the file already held that version. |

A change is `{"id", "agent", "kind", "name", "path", "op", "old_hash", "new_hash", "by", "tool", "session", "tainted", "note", "restores", "at"}`. `op` is `created`, `updated` or `deleted`; `by` is `agent`, `user`, `review`, `curator`, `retro`, `hand` or `system`; `tainted` is true when external content had entered the turn that made it.

### Retros

An agent's [retrospectives](../agents/retros.md), read only. Approve or reject proposals with `habakkuk retro apply` or `/retro apply` in a chat.

| Request | Body | |
|---|---|---|
| `GET /api/retros/{agent}?limit=` | | Retros, newest first, `{"retros": [...]}`, without their evidence. |
| `GET /api/retros/{agent}/{id}` | | One retro with its `evidence`. `id` may be `latest`. |

A retro is `{"id", "agent", "from", "to", "on_demand", "plan", "evidence", "actions", "report", "delivered", "done", "summary", "created_at"}`. `plan` holds `summary`, `went_well`, `went_badly` (each `{"what", "cause", "evidence"}`), `follow_up` and `dropped` (what validation threw out). Each action is `{"n", "kind", "target", "description", "find", "change", "why", "expected", "risk", "evidence", "status", "result", "updated_at"}`; `status` is `proposed`, `applied`, `approved`, `rejected` or `failed`. `report` is the Markdown report's path under `$HABAKKUK_HOME`.

## Skills

| Request | |
|---|---|
| `GET /api/skills/{agent}` | `{"skills": [{"name", "description", "shared"}]}`: the agent's own skills and the shared ones, an agent's own winning a name clash. |
| `GET /api/skills/{agent}/{name}` | `{"name", "description", "shared", "body", "files"}`: the SKILL.md instructions and the supporting files' paths. |

## Usage

### `GET /api/usage?days=30&agent=`

Tokens and cost of every successful model call over the last `days` days (1 to 400), today included.

```json
{
  "since": "2026-10-09", "timezone": "America/Toronto",
  "total": {"key": "total", "calls": 120, "input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0, "cache_write_tokens": 0, "cost": 4.12, "unpriced": 0},
  "days": [{"key": "2026-10-09", "...": "..."}],
  "by_model": [{"key": "anthropic/claude-sonnet-5-5", "...": "..."}],
  "by_agent": [{"key": "habakkuk", "...": "..."}]
}
```

`days` has one row per day, zero rows included. Cost is in dollars, from config `pricing:` and the built-in table; calls through a subscription login cost 0; `unpriced` counts calls whose model has no known price (their cost is not included).

### `GET /api/budget?agent=`

Spend today and this month against the spending limits (config `budget:`), for the whole install and each agent; `agent` keeps one. A `limit` of 0 (left out) is no limit; `paused` is set while a limit is used up and holds the message turns end with.

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

Tool calls waiting for a yes or no in a chat (permission mode `ask`, or `workspace` for exec, or a reviewer that was unsure).

| Request | |
|---|---|
| `GET /api/approvals` | `{"approvals": [{"id", "to": {"account", "conversation", "thread"}, "session", "tool", "summary", "review", "since"}]}`, oldest first. |
| `POST /api/approvals/{id}` `{"approve": true}` | Allows (or with `false` denies) the call, exactly as answering in the chat would, and posts "Approved from the dashboard." there. `404` when it is no longer waiting (answered, timed out, or the turn stopped). |

## Nodes

Present when the config has a `nodes` section. Devices themselves use `/nodes` (see [ARCHITECTURE.md](architecture.md#device-nodes)), not this API.

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

`config` is the effective configuration (defaults applied), read-only, with secrets redacted:

- the value of every variable the config names (`*_env` fields, MCP `env` maps, `${VAR}` in MCP headers) is replaced with `[redacted]` wherever it appears;
- string fields whose names read like secrets (token, password, key, ...) and do not name a variable, and MCP header values that are not variable references, are replaced whole;
- passwords in URLs, secret-looking query parameters, and strings shaped like well-known API keys, bearer tokens, JWTs and private keys are replaced.

`doctor` checks what `habakkuk doctor` checks that the running process can see: provider keys and logins, `control.db` and `transcripts.db` integrity, channel, HTTP and MCP credentials (by variable name, never value), and MCP servers that failed to connect.
