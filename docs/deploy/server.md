---
title: Run it on a server
nav_title: Server
description: "Keep habakkuk serve running under systemd, launchd or Docker: the setup service command, unit files, container volumes, ports and keys."
order: 1
---

## Under a service manager

Habakkuk does not install a service itself. `habakkuk setup service` writes the file and prints the commands that install it:

```bash
habakkuk setup service
```

```bash tab="Linux (systemd)"
# setup service writes $HABAKKUK_HOME/habakkuk.service and prints:
mkdir -p ~/.config/systemd/user && cp ~/.habakkuk/habakkuk.service ~/.config/systemd/user/habakkuk.service
systemctl --user daemon-reload && systemctl --user enable --now habakkuk
loginctl enable-linger "$USER"    # keeps it running while you are logged out
```

```bash tab="macOS (launchd)"
# setup service writes $HABAKKUK_HOME/ai.habakkuk.agent.plist and prints:
mkdir -p ~/Library/LaunchAgents && cp ~/.habakkuk/ai.habakkuk.agent.plist ~/Library/LaunchAgents/ai.habakkuk.agent.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/ai.habakkuk.agent.plist
```

The files run `habakkuk serve` with your `HABAKKUK_HOME` and `PATH`, and restart it when it fails. On macOS its output goes to `serve.out.log` and `serve.err.log` in `$HABAKKUK_HOME`.

> [!IMPORTANT]
> Stop any `habakkuk serve` running in a terminal first: most channels allow only one program to poll them.

### Writing the unit by hand

A minimal systemd user unit, in `~/.config/systemd/user/habakkuk.service`:

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

Set `Environment=HABAKKUK_HOME=...` in the unit to use another home.

### Windows

`habakkuk setup service` covers Linux and macOS only. On Windows, run `habakkuk serve` with the service manager or task scheduler you prefer, or run it in WSL.

### The desktop app

The [desktop app](../interfaces/desktop-app.md) can also start `serve` for you, but it stops it when you quit. For an agent that must stay up, run `serve` as a service and let the app connect to it.

## In a container

The image runs `habakkuk serve` with `HABAKKUK_HOME=/data`:

```bash
docker run --rm -it -v habakkuk-data:/data ghcr.io/habakkuk-ai/habakkuk:latest init      # once
docker run -d --name habakkuk --restart unless-stopped -v habakkuk-data:/data \
  -p 127.0.0.1:8484:8484 ghcr.io/habakkuk-ai/habakkuk:latest
```

- Put keys in `/data/.env`.
- Inside a container the HTTP server must listen on a non-loopback address (`listen: 0.0.0.0:8484`) to be reachable through the published port, which needs `http.token_env`.
- For the browser toolset or meetings, build the variant with Chromium: `docker build --target browser -t habakkuk:browser .`

A Compose file does the same:

```yaml
services:
  habakkuk:
    image: ghcr.io/habakkuk-ai/habakkuk:latest
    restart: unless-stopped
    volumes: [habakkuk-data:/data]
    ports: ["127.0.0.1:8484:8484"]
volumes:
  habakkuk-data:
```

## After it is running

```bash
habakkuk status       # serve uptime, channels, queue, today's cost, recent errors
habakkuk logs -f      # follow the log
```

See [Observability](observability.md) for health checks and metrics, and [Backups and updates](backups-and-updates.md) to keep it safe.
