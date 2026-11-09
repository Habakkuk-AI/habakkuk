---
title: Integrations
description: "Connect Habakkuk to other programs: Home Assistant, signed webhooks, Agent2Agent, the OpenAI-compatible API, code editors over ACP, and device nodes."
order: 9
---

| Integration | What it gives you | Needs `http` |
|---|---|---|
| [Home Assistant](#home-assistant) | Agents control your home; events start turns | no |
| [Webhooks](#webhooks) | Any service starts a turn with a signed POST | yes |
| [Agent2Agent](#agent2agent) | Serve your agents to A2A clients, and message agents elsewhere | to serve |
| [OpenAI-compatible API](#openai-compatible-api) | Any OpenAI client talks to an agent | yes |
| [Code editors (ACP)](#code-editors-acp) | An agent as your editor's coding agent | no |
| [Device nodes](#device-nodes) | Phones and laptops lend notifications, screenshots, a camera, commands | yes |

## Home Assistant

```yaml
agents:
  habakkuk:
    toolsets: [core, memory, skills, homeassistant]
homeassistant:
  url: http://homeassistant.local:8123
  token_env: HA_TOKEN               # a long-lived access token
  allow_domains: [light, switch, climate, media_player]
  confirm: [lock, alarm_control_panel, cover]   # every call asks you first
  triggers:
    - name: doorbell
      entity_id: binary_sensor.front_doorbell
      to: "on"
      prompt: "Someone rang the doorbell at {{.Time}}. Check the porch camera and tell me who it is."
      deliver: telegram:123456789
```

Agents get the tools `ha_states`, `ha_call_service` and `ha_history` by listing the toolset `homeassistant`.

- A service call is allowed only when its domain and every target entity's domain are in `allow_domains` or `confirm`.
- A `confirm` domain asks a human for every call, in every permission mode.
- `triggers` follow Home Assistant's event bus (only in `habakkuk serve`) and start a turn in a fresh session when an event matches, with the reply going to `deliver`.
- What Home Assistant returns counts as outside content.

[Architecture: Home Assistant](../reference/architecture.md#home-assistant) has the matching rules.

## Webhooks

With an [`http` section](../interfaces/dashboard.md), `webhooks` turns an authenticated POST into a turn:

```yaml
http:
  listen: 127.0.0.1:8484
  token_env: HABAKKUK_HTTP_TOKEN
  webhooks:
    alerts:                         # POST /hooks/alerts
      agent: habakkuk               # default: the default agent
      secret_env: ALERTS_SECRET     # HMAC-SHA256 of the body, or a bearer secret
      prompt: "Alert {{.title}} fired. Look into it:\n{{.Body}}"
      deliver: telegram:123456789   # optional; default the web conversation hook-alerts
```

A sender signs the raw body with the secret and may name the delivery so retries run once:

```bash
body='{"title":"disk full"}'
sig=$(printf %s "$body" | openssl dgst -sha256 -hmac "$ALERTS_SECRET" -r | cut -d' ' -f1)
curl -s -H "X-Habakkuk-Signature: sha256=$sig" -H 'X-Habakkuk-Delivery: evt-42' -d "$body" localhost:8484/hooks/alerts
```

Each delivery is a fresh, unattended session. Its prompt is the hook's template over `{{.Body}}` and the body's top-level JSON fields, framed as outside data rather than instructions. The server answers 202 once the turn is queued; the reply goes to `deliver` (`ACCOUNT:CONVERSATION`) or to the web conversation `hook-<name>`.

## Agent2Agent

```yaml
a2a:
  serve: true                       # Agent Card at /.well-known/agent-card.json, JSON-RPC at /a2a (needs http)
  token_env: HABAKKUK_A2A_TOKEN     # default: the http token
  peers:
    research:                       # a2a_send(to: research, message: ...)
      url: https://agents.example.com/research
      headers: {Authorization: "Bearer ${RESEARCH_AGENT_TOKEN}"}
```

Other settings: `name` and `description` (for the Agent Card) and `agents` (which agents to serve).

- Agents get the tools `a2a_discover` and `a2a_send` by listing the toolset `a2a`.
- Habakkuk serves the Agent2Agent protocol in both versions in use (1.0 and 0.3). Each served agent is a skill on the Agent Card and also has its own card and endpoint at `/a2a/<agent>`.
- Sending to a URL that is not a configured peer needs approval below `full` mode.

[Architecture: Agent2Agent](../reference/architecture.md#agent2agent) has the protocol mapping.

## OpenAI-compatible API

With the `http` section, `GET /v1/models` lists one model per agent, `habakkuk/<agent>`, and `POST /v1/chat/completions` and `POST /v1/responses` answer whole or streamed, with the same bearer token. Any OpenAI client can talk to an agent this way:

```bash
curl -s -H "Authorization: Bearer $HABAKKUK_HTTP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"habakkuk/habakkuk","messages":[{"role":"user","content":"what is on today?"}]}' \
  localhost:8484/v1/chat/completions
```

- The agent runs with its own tools; a request with `tools` gets a 400.
- Nobody can approve calls, so ones that need approval are denied.
- `X-Habakkuk-Session`, the `user` field or `previous_response_id` continue a session; every answer carries `X-Habakkuk-Session`.
- `Idempotency-Key` makes a retry attach to the first request's turn.

See [Architecture: other clients](../reference/architecture.md#other-clients).

## Code editors (ACP)

```bash
habakkuk acp [-agent A]
```

`habakkuk acp` speaks the Agent Client Protocol on stdio, so editors that support ACP can use a Habakkuk agent as their coding agent. Configure your editor to run that command.

- The session's working folder is its workspace.
- Model text, tool calls and plans stream to the editor.
- Approvals become the editor's permission prompts.
- File reads and writes go through the editor when it offers them, so they see unsaved buffers.

## Device nodes

Other machines lend Habakkuk capabilities: notifications, screenshots, location, the clipboard, and when you allow it, running commands, the camera and a folder of files. A `nodes` section (which needs `http`) turns it on:

```bash
habakkuk node pair                       # on the server: a one-time code and a QR code for phones
habakkuk node run -server URL -code CODE -name laptop [-allow-run] [-allow-camera] [-dir DIR]   # on the device
habakkuk nodes                           # paired nodes
habakkuk nodes revoke laptop
```

- A browser can be a node too, through the dashboard or `/app/pair.html` on a phone, offering notifications, location and a camera snapshot behind the browser's own permission prompts.
- Agents get `nodes_list` and `node_invoke` in the `core` toolset.
- A command not listed for that node in `nodes.allow` goes through the usual approval.
- What a device returns counts as outside content.

Other `nodes` settings: `url` (the address devices use), `timeout` and `pair_ttl`. [Architecture: device nodes](../reference/architecture.md#device-nodes) has the protocol.
