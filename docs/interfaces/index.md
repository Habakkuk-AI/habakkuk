---
title: Interfaces
description: "The ways to use Habakkuk besides chat apps: the terminal UI, one-shot commands, the web dashboard, the desktop app, voice, editors and APIs."
order: 6
---

Chat apps are one way in. Habakkuk also has its own interfaces, all talking to the same agents, sessions and memory.

| Interface | Best for | Page |
|---|---|---|
| `habakkuk chat` / `habakkuk tui` | A full-screen chat in the terminal, with tool output and approvals | [CLI and TUI](cli-and-tui.md) |
| `habakkuk run` | One turn from a script or cron job | [CLI and TUI](cli-and-tui.md#one-shot-runs) |
| `hk -r NAME ...` | Any command on your laptop, run against the Habakkuk on your server | [Remote CLI](remote-cli.md) |
| The dashboard at `/app/` | Chat, sessions, board, schedules, memory, usage and approvals in a browser | [Dashboard](dashboard.md) |
| Habakkuk Desktop | A native window with a tray icon, notifications and a quick-ask hotkey | [Desktop app](desktop-app.md) |
| The chat page at `/` | A light chat on a phone or laptop | [Web chat](../channels/web-chat.md) |
| Live mode and `habakkuk talk` | Talking out loud | [Voice](../tools/voice.md) |
| `habakkuk acp` | Your code editor's coding agent | [Integrations](../tools/integrations.md#code-editors-acp) |
| The OpenAI-compatible API, MCP and A2A | Other programs and agents | [Integrations](../tools/integrations.md) |

## In this section

<!-- cards -->
- [CLI and TUI](cli-and-tui.md): the terminal chat, its keys and commands, and one-shot runs.
- [Dashboard](dashboard.md): the `http` section, the control dashboard and the other endpoints it serves.
- [Desktop app](desktop-app.md): Habakkuk Desktop for macOS, Windows and Linux.
- [Remote CLI](remote-cli.md): `hk -r NAME` runs commands on the Habakkuk on your server.
