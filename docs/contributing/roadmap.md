---
title: Roadmap
description: "Release phases, what each one ships, and known gaps."
order: 3
---

The repo stays private until 0.3, then opens under the Apache License 2.0.

| Phase | Ships | Done when |
|---|---|---|
| **0.1 Core** (done) | Turn loop, Anthropic and OpenAI-compatible providers with retry, key rotation and fallback, tool registry and permission modes, durable job queue, split storage with retention, CLI | A turn with tools survives `kill -9` mid-tool and resumes on restart (verified live 2026-11-06) |
| **0.2 Channels** (built) | Slack (multi-workspace Socket Mode), WhatsApp (linked device), Telegram; routes from accounts and conversations to agents; pairing codes; chat approvals; per-channel supervisor with heartbeat watchdog; idempotent turns and resumable deliveries; transactional credentials with rotating backups | Cutting the network or filling the disk never corrupts credentials; Slack reconnects without restarting the process |
| 0.3 Org | Multiple agents with their own SOUL files; board (built: cards, claims, proof, dependencies, results back to the creator); subagent delegation as jobs (built); schedules with at-most-once slots (built); script schedules that run a command without the model and post its output only when there is something to say (built); LLM reviewer for exec approvals (built: `approvals: reviewer` or `reviewer_then_human`, fixed strict prompt, malformed answers ask, decisions logged and in the transcript); release builds (built: static binaries, checksums, ghcr.io image and `install.sh` on each `v*` tag) | One agent assigns a card, another completes it across a restart |
| 0.4 Knowledge (memory and skills built) | Memory (built: Markdown notes as the source of truth, FTS5 index with bm25 rebuilt from file mtimes, prompt snapshot, save/search/read/forget tools; semantic search over embeddings fused with FTS, USER.md profile, background review and nightly consolidation, provenance and untrusted marking); `SKILL.md` skills (built: shared and per-agent folders with agent precedence, prompt snapshot, view/file/save tools with backups; later: idle-time curator); MCP client (built: stdio and Streamable HTTP servers, toolset per server, reconnect with backoff, per-server risk, approve and allow lists) | Cross-session recall test passes; existing Habakkuk skills load unchanged |
| 0.5 Migration | Importer for existing agent setups (personas, skills, schedules, task boards, MCP servers); shadow mode beside an existing agent | A week in shadow mode with no missed or duplicated replies |

## Known gaps

- Replies arrive when the turn finishes; there is no streaming of partial text into the chat. Progress shows as a draft message edited with each tool step (Telegram, Slack; `progress: draft`, the default), a typing indicator (Telegram, WhatsApp), or one "still working" notice after 60 seconds (Slack with `progress: off`).
- Steering notes (`/steer`) taken by a turn that then loses its lease before writing them are lost; the window is one model call.
- `send_file` delivery is at-least-once at the last step: a crash after the platform accepted a file but before the job completed sends it again. A model that repeats the call after a restart (a new tool call ID) also sends it again.
- Script schedules are added with the CLI only; they cannot yet be declared in `config.yaml`, and there is no command to edit one in place (remove and add again).

## Known gaps on Windows

Windows builds are new and have only run on CI so far. Two things need someone with a Windows machine: the browser does not follow popups opened by a page (the port-based launch Windows uses), and the dashboard's live activity stream timed out in one CI test. Both tests are skipped on Windows with a pointer here. Computer use is not supported on Windows yet.
