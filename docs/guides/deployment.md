---
title: Deployment and running as a service
description: "Keep habakkuk serve running on a server or a home machine: containers, a service manager, remote access, monitoring, logs, secrets, security audit, backups and updates."
order: 18
---

`habakkuk serve` is the long-running process: it runs the channels, the job workers (resuming interrupted turns), schedules, the board, background memory review, retention, and the HTTP server when there is an `http` section. Run one `serve` per `$HABAKKUK_HOME`. It stops cleanly on `SIGINT` or `SIGTERM`, and anything in flight resumes on the next start.

## In a container

The image runs `habakkuk serve` with `HABAKKUK_HOME=/data`:

```bash
docker run --rm -it -v habakkuk-data:/data ghcr.io/habakkuk-ai/habakkuk:latest init      # once
docker run -d --name habakkuk --restart unless-stopped -v habakkuk-data:/data \
  -p 127.0.0.1:8484:8484 ghcr.io/habakkuk-ai/habakkuk:latest
```

Put keys in `/data/.env`. Inside a container the HTTP server must listen on a non-loopback address to be reachable through the published port, which needs `http.token_env`. For the browser toolset or meetings, build the variant with Chromium: `docker build --target browser -t habakkuk:browser .`

## Under a service manager

Habakkuk does not install a service itself; use your system's service manager. A systemd user unit, for example, in `~/.config/systemd/user/habakkuk.service`:

```ini
[Unit]
Description=Habakkuk
After=network-online.target

[Service]
ExecStart=%h/.local/bin/habakkuk serve
Restart=on-failure

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now habakkuk
loginctl enable-linger "$USER"     # keep it running after you log out
```

Set `Environment=HABAKKUK_HOME=...` in the unit to use another home. On macOS, a LaunchAgent that runs `habakkuk serve` with `KeepAlive` does the same. The [desktop app](desktop-app.md) can also start `serve` for you, but stops it when you quit; for an agent that must stay up, run `serve` as a service and let the app connect to it.

## Status and health

```bash
habakkuk status                          # serve uptime, channels, queue, today's cost, recent errors
habakkuk channels                        # channel health
habakkuk doctor                          # config, credentials and storage
curl -s localhost:8484/healthz           # no token: channel states and queue depth
```

`habakkuk serve` writes `serve.json` (pid, version, start, listen address) every 15 seconds and removes it on exit; `habakkuk status` uses it to tell whether serve is running.

## Remote access

Webhook channels and remote dashboards need the HTTP server reachable. Keep `listen` on loopback and put a reverse proxy or tunnel in front, with `token_env` set and `public_url` naming the outside address. On a tailnet:

```bash
habakkuk expose tailscale                # share the HTTP listener on your tailnet (never public)
habakkuk expose status
habakkuk expose off
```

`habakkuk expose tailscale [-port 443] [-print]` runs `tailscale serve`, which shares the listener with the tailnet only; Habakkuk never runs `tailscale funnel`. It refuses without `http.token_env`.

## Metrics, traces and logs

```bash
habakkuk logs -f -level warn -since 1h   # readable habakkuk.log; -grep filters
```

```yaml
metrics: {public: false}          # GET /metrics (Prometheus) needs the API token unless public
telemetry:                        # optional OTLP/HTTP traces: a span per turn, model call and tool call
  otlp_endpoint: http://localhost:4318
  headers_env: OTLP_HEADERS       # "key=value,key2=value2"
  include_content: false          # message text and tool I/O stay out of spans by default
logging: {max_mb: 20, backups: 5} # habakkuk.log rotation; secrets are redacted before writing
```

[Architecture: operations](../reference/architecture.md#operations) lists the metric families and what the log redactor removes.

## Secret managers

```yaml
secrets:                          # resolve *_env variables from secret managers
  sources:
    1password: {}                 # op read
    bitwarden: {}                 # bw get (ITEM, or ITEM#field)
    file: {}
    pass: {type: command, command: 'pass show "$HABAKKUK_SECRET_REF"', cache: 10m}
  refs:
    ANTHROPIC_API_KEY: op://Private/Anthropic/credential
    TELEGRAM_BOT_TOKEN: pass:habakkuk/telegram
```

A variable is read from the process environment first, then a reference, then `.env`.

## Security audit

```bash
habakkuk security audit -fix             # permissions, exposure, risky settings; -fix applies safe fixes
```

It checks file modes under `$HABAKKUK_HOME`, agents in `full` mode or reachable by outsiders, open channels, the HTTP listener and its token, webhook secrets, risky MCP servers, skills and hooks others can write, backups and free disk space. It exits non-zero while a high or critical finding remains, so it can run in a check.

## Backups

```bash
habakkuk backup -o habakkuk.tar.gz       # config, agents, skills and database snapshots (-env adds secrets)
habakkuk restore habakkuk.tar.gz         # unpack a backup into $HABAKKUK_HOME
```

`habakkuk backup [-o FILE] [-env] [-workspaces]` takes consistent database snapshots while Habakkuk runs. `-env` adds `.env` and the WhatsApp credential databases; keep such archives private. `habakkuk restore [-force] FILE` refuses to overwrite an existing config without `-force`; stop `habakkuk serve` first.

## Updates

```bash
habakkuk update -check                   # is there a newer release?
habakkuk update                          # install it (checksum verified)
```

Restart `serve` after updating. Container users pull the new image instead.

## Retention

```yaml
retention:
  days: 90      # transcripts and finished jobs older than this are pruned; 0 disables
```
