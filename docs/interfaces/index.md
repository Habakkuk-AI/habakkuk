---
title: Interfaces
description: "Use Habakkuk without a chat app: the terminal UI, one-shot commands, the web dashboard, the desktop app, voice, editors and APIs."
order: 6
---

Chat apps are one way to reach your agents. Habakkuk also has interfaces of its own, and they all talk to the same agents, sessions and memory. Use the table to pick one, then follow the link to its page.

| Interface | Best for | Page |
|---|---|---|
| `hk chat` / `hk tui` | A full-screen chat in the terminal, with tool output and approvals | [CLI and TUI](cli-and-tui.md) |
| `hk run` | One turn from a script or cron job | [CLI and TUI](cli-and-tui.md#one-shot-runs) |
| `hk -r NAME ...` | Any command on your laptop, run against the Habakkuk on your server | [Remote CLI](remote-cli.md) |
| The dashboard at `/app/` | Chat, sessions, board, schedules, memory, usage and approvals in a browser | [Dashboard](dashboard.md) |
| Habakkuk Desktop | A native window with a tray icon, notifications and a quick-ask hotkey | [Desktop app](desktop-app.md) |
| The chat page at `/` | A light chat on a phone or laptop | [Web chat](../channels/web-chat.md) |
| Live mode and `hk talk` | Talking out loud | [Voice](../tools/voice.md) |
| `hk acp` | Your code editor's coding agent | [Integrations](../tools/integrations.md#code-editors-acp) |
| The OpenAI-compatible API, MCP and A2A | Other programs and agents | [Integrations](../tools/integrations.md) |

## In this section

<!-- cards -->
- [CLI and TUI](cli-and-tui.md): chat in the terminal, learn its keys and commands, and do one-shot runs.
- [Dashboard](dashboard.md): add the `http` section to get the control dashboard and the other endpoints it serves.
- [Desktop app](desktop-app.md): get Habakkuk Desktop for macOS, Windows and Linux.
- [Remote CLI](remote-cli.md): use `hk -r NAME` to run commands on the Habakkuk on your server.
