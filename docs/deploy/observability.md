---
title: Observability
description: "Check what Habakkuk is doing with status, channel health, doctor, the health endpoint, the log, Prometheus metrics and OpenTelemetry traces."
order: 5
---

There are several ways to see what Habakkuk is doing: status and health checks, the log, Prometheus metrics, OpenTelemetry traces, and usage and cost.

## Status and health

```bash
hk status                                # version, serve uptime, channels, queue, recent errors, today's cost
hk channels                              # channel health
hk doctor                                # config, credentials, storage, channels, MCP servers
hk doctor --fix                          # the same, after making the safe fixes for you
curl -s localhost:8484/healthz           # no token: channel states and queue depth
```

- `hk serve` writes `serve.json` (pid, version, start, listen address) as soon as it starts, and again every 15 seconds. It removes the file on exit. `hk status` reads it to tell you whether `serve` is running, or still starting and what it's waiting for. Two log lines time the start. `first channel connected` gives the time from start to the first channel. `serve ready` gives the time until every channel has made its first attempt.
- Send `/status` in a chat to see the agent, session and channel health from where you are.
- When `hk doctor` finds nothing wrong, it ends by telling you the next step. If `serve` is already running, still starting, or alive but quiet, it says so instead of telling you to start it.
- When `hk doctor` finds a problem, it exits non-zero and names the problem. That can be a missing key (`MISSING ...`), an unknown config key, a provider that isn't logged in, or a database that fails its check.
- `hk doctor --fix` first makes the fixes that need no decision from you, all inside `$HABAKKUK_HOME` except the service file: an older config format, missing folders, files other users can read, and a background service that points at a binary that's gone. It prints a `fixed:` line for each. See [Troubleshooting](../help/troubleshooting.md).

## Logs

```bash
hk logs -f -level warn -since 1h         # follow habakkuk.log, readably
hk logs -grep telegram -n 100            # filter, and how many lines
```

Habakkuk rotates `$HABAKKUK_HOME/habakkuk.log`, and it redacts secrets before anything is written. This setting controls the rotation:

```yaml
logging: {max_mb: 20, backups: 5}   # habakkuk.log rotation
```

## Metrics and traces

```yaml
metrics: {public: false}          # GET /metrics (Prometheus) needs the API token unless public
telemetry:                        # optional OTLP/HTTP traces: a span per turn, model call and tool call
  otlp_endpoint: http://localhost:4318
  headers_env: OTLP_HEADERS       # "key=value,key2=value2"
  include_content: false          # message text and tool I/O stay out of spans by default
  service_name: habakkuk
```

The [HTTP server](../interfaces/dashboard.md) serves `/metrics`, so you need an `http` section in your config to use it. Traces go only to the endpoint you name. Habakkuk sends no telemetry of its own.

`habakkuk_loop_guard_total{agent,check,action}` counts the times the [loop guard](../agents/agents.md#when-an-agent-goes-round-in-circles) warned an agent that was repeating itself (`action="warn"`) or stopped its turn (`action="stop"`). A rising stop count usually means a tool is broken or an instruction is unclear.

For the metric families and what the log redactor removes, see [Architecture: operations](../reference/architecture.md#operations).

## Usage and cost

Run `hk usage` to see tokens and cost by model and by agent, and how much you've spent against your limits. See [Usage and limits](../models/usage-and-limits.md).
