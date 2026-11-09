---
title: Observability
description: "See what Habakkuk is doing: status, channel health, doctor, the health endpoint, the log, Prometheus metrics and OpenTelemetry traces."
order: 5
---

## Status and health

```bash
habakkuk status                          # version, serve uptime, channels, queue, recent errors, today's cost
habakkuk channels                        # channel health
habakkuk doctor                          # config, credentials, storage, channels, MCP servers
curl -s localhost:8484/healthz           # no token: channel states and queue depth
```

- `habakkuk serve` writes `serve.json` (pid, version, start, listen address) every 15 seconds and removes it on exit. `habakkuk status` uses it to tell whether `serve` is running.
- `/status` in a chat shows the agent, session and channel health from where you are.
- `habakkuk doctor` exits non-zero when it finds a problem, and names it: a missing key (`MISSING ...`), an unknown config key, a provider that is not logged in, a database that fails its check.

## Logs

```bash
habakkuk logs -f -level warn -since 1h   # follow habakkuk.log, readably
habakkuk logs -grep telegram -n 100      # filter, and how many lines
```

`$HABAKKUK_HOME/habakkuk.log` is rotated, and secrets are redacted before anything is written:

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

`/metrics` is served by the [HTTP server](../interfaces/dashboard.md), so it needs an `http` section. Traces go only to the endpoint you name; Habakkuk sends no telemetry of its own.

[Architecture: operations](../reference/architecture.md#operations) lists the metric families and what the log redactor removes.

## Usage and cost

`habakkuk usage` shows tokens and cost by model and agent, and spend against your limits. See [Usage and limits](../models/usage-and-limits.md).
