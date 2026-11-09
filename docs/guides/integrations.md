---
title: Integrations
description: "Connect Habakkuk to other programs: Agent2Agent, Home Assistant, the OpenAI-compatible API, code editors over ACP, and device nodes."
order: 17
---

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

Agents get the tools (`a2a_discover`, `a2a_send`) by listing the toolset `a2a`. Habakkuk serves the Agent2Agent protocol in both versions in use (1.0 and 0.3). Each served agent is a skill on the Agent Card and also has its own card and endpoint at `/a2a/<agent>`. Sending to a URL that is not a configured peer needs approval below `full` mode. [Architecture: Agent2Agent](../reference/architecture.md#agent2agent) has the protocol mapping.

## Home Assistant

```yaml
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

Agents get the tools (`ha_states`, `ha_call_service`, `ha_history`) by listing the toolset `homeassistant`. A service call is allowed only when its domain and every target entity's domain are in `allow_domains` or `confirm`; a `confirm` domain asks a human for every call in every permission mode. `triggers` follow Home Assistant's event bus (only in `habakkuk serve`) and start a turn in a fresh session when an event matches, with the reply going to `deliver`. [Architecture: Home Assistant](../reference/architecture.md#home-assistant) has the matching rules.

## OpenAI-compatible API

With the `http` section, `GET /v1/models` lists one model per agent, `habakkuk/<agent>`, and `POST /v1/chat/completions` and `POST /v1/responses` answer whole or streamed, with the same bearer token. Any OpenAI client can talk to an agent this way:

```bash
curl -s -H "Authorization: Bearer $HABAKKUK_HTTP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"habakkuk/habakkuk","messages":[{"role":"user","content":"what is on today?"}]}' \
  localhost:8484/v1/chat/completions
```

The agent runs with its own tools (a request with `tools` gets a 400), and since nobody can approve calls, ones that need approval are denied. `X-Habakkuk-Session`, the `user` field or `previous_response_id` continue a session; every answer carries `X-Habakkuk-Session`. `Idempotency-Key` makes a retry attach to the first request's turn. See [Architecture: other clients](../reference/architecture.md#other-clients).

## Code editors (ACP)

`habakkuk acp [-agent A]` speaks the Agent Client Protocol on stdio, so editors that support ACP can use a Habakkuk agent as their coding agent. The session's working folder is its workspace; model text, tool calls and plans stream to the editor, approvals become the editor's permission prompts, and file reads and writes go through the editor when it offers them, so they see unsaved buffers.

## Device nodes

Other machines lend Habakkuk capabilities: notifications, screenshots, location, the clipboard, and when you allow it, running commands, the camera and a folder of files. A `nodes` section (which needs `http`) turns it on:

```bash
habakkuk node pair                       # on the server: a one-time code and a QR code for phones
habakkuk node run -server URL -code CODE -name laptop [-allow-run] [-allow-camera] [-dir DIR]   # on the device
habakkuk nodes                           # paired nodes
habakkuk nodes revoke laptop
```

A browser can be a node too, through the dashboard or `/app/pair.html` on a phone, offering notifications, location and a camera snapshot behind the browser's own permission prompts. Agents get `nodes_list` and `node_invoke` in the `core` toolset; a command not listed for that node in `nodes.allow` goes through the usual approval. What a device returns counts as outside content. [Architecture: device nodes](../reference/architecture.md#device-nodes) has the protocol.
