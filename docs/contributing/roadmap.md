---
title: Roadmap
description: "The release phases, what ships in each one, and the known gaps."
order: 3
---

Habakkuk ships in phases. Below is what goes into each one, and the gaps that are already known. The repo stays private until 0.3. Then it opens under the Apache License 2.0.

| Phase | Ships | Done when |
|---|---|---|
| **0.1 Core** (done) | Turn loop, Anthropic and OpenAI-compatible providers with retry, key rotation and fallback, tool registry and permission modes, durable job queue, split storage with retention, CLI | A turn with tools survives `kill -9` mid-tool and resumes on restart (verified live 2026-11-06) |
| **0.2 Channels** (built) | Slack (multi-workspace Socket Mode), WhatsApp (linked device), Telegram; routes from accounts and conversations to agents; pairing codes; chat approvals; per-channel supervisor with heartbeat watchdog; idempotent turns and resumable deliveries; transactional credentials with rotating backups | Cutting the network or filling the disk never corrupts credentials; Slack reconnects without restarting the process |
| 0.3 Org | Multiple agents with their own SOUL files; board (built: cards, claims, proof, dependencies, results back to the creator); subagent delegation as jobs (built); schedules with at-most-once slots (built); script schedules that run a command without the model and post its output only when there is something to say (built); LLM reviewer for exec approvals (built: `approvals: reviewer` or `reviewer_then_human`, fixed strict prompt, malformed answers ask, decisions logged and in the transcript); release builds (built: static binaries, checksums, ghcr.io image and `install.sh` on each `v*` tag) | One agent assigns a card, another completes it across a restart |
| 0.4 Knowledge (memory and skills built) | Memory (built: Markdown notes as the source of truth, FTS5 index with bm25 rebuilt from file mtimes, prompt snapshot, save/search/read/forget tools; semantic search over embeddings fused with FTS, USER.md profile, background review and nightly consolidation, provenance and untrusted marking); `SKILL.md` skills (built: shared and per-agent folders with agent precedence, prompt snapshot, view/file/save tools with backups; later: idle-time curator); MCP client (built: stdio and Streamable HTTP servers, toolset per server, reconnect with backoff, per-server risk, approve and allow lists) | Cross-session recall test passes; existing Habakkuk skills load unchanged |
| 0.5 Migration | Importer for existing agent setups (personas, skills, schedules, task boards, MCP servers); shadow mode beside an existing agent | A week in shadow mode with no missed or duplicated replies |

## Known gaps

- A reply arrives when the turn finishes. Partial text isn't streamed into the chat. Progress shows up as a draft message that's edited with each tool step (Telegram, Slack; `progress: draft`, the default), a typing indicator (Telegram, WhatsApp), or one "still working" notice after 60 seconds (Slack with `progress: off`).
- A steering note (`/steer`) is lost if a turn takes it and then loses its lease before writing it. The window for this is one model call.
- `send_file` delivery is at-least-once at the last step. If a crash happens after the platform accepted a file but before the job completed, the file is sent again. It's also sent again if a model repeats the call after a restart (a new tool call ID).
- You can add script schedules with the CLI only. You can't declare them in `config.yaml` yet. There's no command to edit one in place, so remove it and add it again.

## Known gaps on Windows

Windows builds are new, and so far they've only run on CI. Two things need someone with a Windows machine. First, the browser doesn't follow popups opened by a page (the port-based launch Windows uses). Second, the dashboard's live activity stream timed out in one CI test. Both tests are skipped on Windows, with a pointer here. Computer use isn't supported on Windows yet.
