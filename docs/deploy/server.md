---
title: Run it on a server
nav_title: Server
description: "Keep habakkuk serve running under systemd, launchd or Docker, with the setup service command, unit files, container volumes, ports and keys."
order: 1
---

To keep `habakkuk serve` running on a machine that stays on, run it under a service manager (systemd or launchd) or in a Docker container.

## Under a service manager

Habakkuk doesn't install a service for you. Run `habakkuk setup service` instead. It writes the service file and prints the commands that install it:

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

These files run `habakkuk serve` with your `HABAKKUK_HOME` and `PATH`, and they restart it if it fails. On macOS, its output goes to `serve.out.log` and `serve.err.log` in `$HABAKKUK_HOME`.

> [!IMPORTANT]
> First stop any `habakkuk serve` that's running in a terminal. Most channels let only one program poll them.

### Writing the unit by hand

Here's a minimal systemd user unit. It goes in `~/.config/systemd/user/habakkuk.service`:

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

To use a different home, set `Environment=HABAKKUK_HOME=...` in the unit.

### Windows

`habakkuk setup service` only covers Linux and macOS. On Windows, run `habakkuk serve` with whichever service manager or task scheduler you prefer, or run it in WSL.

### The desktop app

The [desktop app](../interfaces/desktop-app.md) can start `serve` for you too, but it stops `serve` when you quit the app. If your agent has to stay up, run `serve` as a service and let the app connect to it.

## In a container

The container image runs `habakkuk serve` and sets `HABAKKUK_HOME=/data`:

```bash
docker run --rm -it -v habakkuk-data:/data ghcr.io/habakkuk-ai/habakkuk:latest init      # once
docker run -d --name habakkuk --restart unless-stopped -v habakkuk-data:/data \
  -p 127.0.0.1:8484:8484 ghcr.io/habakkuk-ai/habakkuk:latest
```

- Put your keys in `/data/.env`.
- Inside a container, the HTTP server is reachable through the published port only if it listens on a non-loopback address (`listen: 0.0.0.0:8484`). Listening there needs `http.token_env`.
- If you want the browser toolset or meetings, build the variant that includes Chromium: `docker build --target browser -t habakkuk:browser .`

You can do the same with a Compose file:

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

For health checks and metrics, see [Observability](observability.md). To keep it safe, see [Backups and updates](backups-and-updates.md).
