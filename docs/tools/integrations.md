---
title: Integrations
description: "Connect Habakkuk to Home Assistant, signed webhooks, Agent2Agent, OpenAI-compatible clients, code editors over ACP, and phones or laptops as device nodes."
order: 9
---

Habakkuk connects to other programs in several ways. Find the integration you want in the table, then jump to its section. The last column tells you whether it needs an `http` section in your config.

| Integration | What it gives you | Needs `http` |
|---|---|---|
| [Home Assistant](#home-assistant) | Agents control your home; events start turns | no |
| [Webhooks](#webhooks) | Any service starts a turn with a signed POST | yes |
| [Agent2Agent](#agent2agent) | Serve your agents to A2A clients, and message agents elsewhere | to serve |
| [OpenAI-compatible API](#openai-compatible-api) | Any OpenAI client talks to an agent | yes |
| [Code editors (ACP)](#code-editors-acp) | An agent as your editor's coding agent | no |
| [Device nodes](#device-nodes) | Phones and laptops lend notifications, screenshots, a camera, commands | yes |

## Home Assistant

This integration lets agents control your home, and lets events in Home Assistant start turns:

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

Add the `homeassistant` toolset to an agent and it gets the tools `ha_states`, `ha_call_service` and `ha_history`.

- A service call goes through only if its domain, and the domain of every entity it targets, is in `allow_domains` or `confirm`.
- Every call to a `confirm` domain asks a human first, whatever the permission mode.
- `triggers` follow Home Assistant's event bus, which only happens in `habakkuk serve`. When an event matches, a turn starts in a fresh session and the reply goes to `deliver`.
- Anything Home Assistant returns is treated as outside content.

For the matching rules, see [Architecture: Home Assistant](../reference/architecture.md#home-assistant).

## Webhooks

A webhook lets another service start a turn by sending an authenticated POST. You need an [`http` section](../interfaces/dashboard.md) in your config. Add `webhooks` to it:

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

The sender signs the raw body with the secret. It can also name the delivery, so that retries of it run only once:

```bash
body='{"title":"disk full"}'
sig=$(printf %s "$body" | openssl dgst -sha256 -hmac "$ALERTS_SECRET" -r | cut -d' ' -f1)
curl -s -H "X-Habakkuk-Signature: sha256=$sig" -H 'X-Habakkuk-Delivery: evt-42' -d "$body" localhost:8484/hooks/alerts
```

Each delivery runs in a fresh session with nobody attending it. Habakkuk builds the prompt from the hook's template, filling in `{{.Body}}` and the body's top-level JSON fields. The prompt frames that content as outside data, so it isn't read as instructions. The server answers 202 as soon as the turn is queued. The reply goes to `deliver` (written as `ACCOUNT:CONVERSATION`), or to the web conversation `hook-<name>`.

## Agent2Agent

With Agent2Agent (A2A), you can serve your agents to A2A clients, and your agents can message agents elsewhere:

```yaml
a2a:
  serve: true                       # Agent Card at /.well-known/agent-card.json, JSON-RPC at /a2a (needs http)
  token_env: HABAKKUK_A2A_TOKEN     # default: the http token
  peers:
    research:                       # a2a_send(to: research, message: ...)
      url: https://agents.example.com/research
      headers: {Authorization: "Bearer ${RESEARCH_AGENT_TOKEN}"}
```

There are three more settings. `name` and `description` go on the Agent Card, and `agents` says which agents to serve.

- Add the `a2a` toolset to an agent and it gets the tools `a2a_discover` and `a2a_send`.
- Habakkuk serves both versions of the Agent2Agent protocol that are in use (1.0 and 0.3). Each agent you serve is a skill on the Agent Card. It also has a card and endpoint of its own at `/a2a/<agent>`.
- Below `full` mode, an agent needs approval to send to a URL that isn't a configured peer.

For the protocol mapping, see [Architecture: Agent2Agent](../reference/architecture.md#agent2agent).

## OpenAI-compatible API

Any OpenAI client can talk to an agent once you have the `http` section. `GET /v1/models` lists one model per agent, named `habakkuk/<agent>`. `POST /v1/chat/completions` and `POST /v1/responses` send the answer back whole or streamed. They all take the same bearer token. For example:

```bash
curl -s -H "Authorization: Bearer $HABAKKUK_HTTP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"habakkuk/habakkuk","messages":[{"role":"user","content":"what is on today?"}]}' \
  localhost:8484/v1/chat/completions
```

- The agent runs with its own tools. A request that includes `tools` gets a 400.
- There's nobody to approve calls, so any call that needs approval is denied.
- To continue a session, send `X-Habakkuk-Session`, the `user` field or `previous_response_id`. Every answer carries `X-Habakkuk-Session`.
- Send `Idempotency-Key` and a retry attaches to the first request's turn.

For more, see [Architecture: other clients](../reference/architecture.md#other-clients).

## Code editors (ACP)

```bash
habakkuk acp [-agent A]
```

`habakkuk acp` speaks the Agent Client Protocol (ACP) on stdio. An editor that supports ACP can then use a Habakkuk agent as its coding agent. Set your editor up to run that command.

- The session's working folder becomes its workspace.
- The editor gets model text, tool calls and plans as a stream.
- Approvals show up as the editor's permission prompts.
- File reads and writes go through the editor when it offers them. That way they see unsaved buffers.

## Device nodes

A device node is another machine, such as a phone or a laptop, that lends Habakkuk some of its capabilities. Those are notifications, screenshots, location and the clipboard. When you allow it, they also include running commands, the camera and a folder of files. A `nodes` section turns this on, and it needs `http`. Then you pair each device:

```bash
habakkuk node pair                       # on the server: a one-time code and a QR code for phones
habakkuk node run -server URL -code CODE -name laptop [-allow-run] [-allow-camera] [-dir DIR]   # on the device
habakkuk nodes                           # paired nodes
habakkuk nodes revoke laptop
```

- A browser can be a node too. Pair it through the dashboard, or through `/app/pair.html` on a phone. It offers notifications, location and a camera snapshot, behind the browser's own permission prompts.
- The `core` toolset gives agents `nodes_list` and `node_invoke`.
- If a command isn't listed for that node in `nodes.allow`, it goes through the usual approval.
- Anything a device returns is treated as outside content.

The `nodes` section has three more settings: `url` (the address devices use), `timeout` and `pair_ttl`. For the protocol, see [Architecture: device nodes](../reference/architecture.md#device-nodes).
