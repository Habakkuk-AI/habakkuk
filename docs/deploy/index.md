---
title: Deploy
description: "Keep habakkuk serve running on a server or a home machine: service managers and containers, remote access, security, backups and updates, and monitoring."
order: 7
---

`habakkuk serve` is the long-running process. It runs the channels, the job workers (resuming interrupted turns), schedules, the board, background memory review, retention, and the HTTP server when there is an `http` section.

- Run **one** `serve` per `$HABAKKUK_HOME`.
- It stops cleanly on `SIGINT` or `SIGTERM`, and anything in flight resumes on the next start.
- A small machine is enough: it is one static binary with SQLite storage, and the models run at your providers (or on a separate model server).

## Pick a setup

| You have | Do this |
|---|---|
| A Linux or macOS machine that stays on | `habakkuk setup service` writes a systemd or launchd file; see [Run it on a server](server.md#under-a-service-manager) |
| Docker | Run the image with a volume for `/data`; see [In a container](server.md#in-a-container) |
| A laptop you close at night | Run `serve` there anyway: work missed while it slept resumes, and late schedule slots are skipped |
| Webhook channels or a remote dashboard | Put it behind HTTPS; see [Remote access](remote-access.md) |

## In this section

<!-- cards -->
- [Run it on a server](server.md): systemd, launchd and Docker, and what `serve` needs.
- [Remote access](remote-access.md): reverse proxies, tunnels and `habakkuk expose tailscale`.
- [Security](security.md): what an agent may do by default, the audit, and secret managers.
- [Backups and updates](backups-and-updates.md): `habakkuk backup`, restore, updates and retention.
- [Observability](observability.md): status, health checks, logs, metrics and traces.
