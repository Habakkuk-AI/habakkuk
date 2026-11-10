---
title: Deploy
description: "Keep hk serve running on a server or a home machine, with service managers or containers, remote access, security, backups, updates and monitoring."
order: 7
---

This section shows you how to keep Habakkuk running all the time, on a server or on a machine at home. The process you keep running is `hk serve`. It runs the channels, the job workers (which resume interrupted turns), schedules, the board, background memory review and retention. It also runs the HTTP server when your config has an `http` section.

- Run only **one** `serve` for each `$HABAKKUK_HOME`.
- It shuts down cleanly on `SIGINT` or `SIGTERM`. Work that was in progress resumes the next time it starts.
- A small machine is enough. Habakkuk is one static binary with SQLite storage, and the models run at your providers (or on a separate model server).

## Pick a setup

| You have | Do this |
|---|---|
| A Linux or macOS machine that stays on | `hk setup service` writes a systemd or launchd file; see [Run it on a server](server.md#under-a-service-manager) |
| Docker | Run the image with a volume for `/data`; see [In a container](server.md#in-a-container) |
| A laptop you close at night | Run `serve` there anyway: work missed while it slept resumes, and late schedule slots are skipped |
| Webhook channels or a remote dashboard | Put it behind HTTPS; see [Remote access](remote-access.md) |

## In this section

<!-- cards -->
- [Run it on a server](server.md): set it up under systemd, launchd or Docker, and see what `serve` needs.
- [Remote access](remote-access.md): reach it from outside through a reverse proxy, a tunnel or `hk expose tailscale`.
- [Security](security.md): what an agent is allowed to do by default, the audit, and secret managers.
- [Backups and updates](backups-and-updates.md): back up with `hk backup`, restore, update, and see how retention works.
- [Observability](observability.md): check status and health, read the logs, and collect metrics and traces.
