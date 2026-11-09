---
title: Use cases
description: "Eight concrete setups, from a personal assistant on WhatsApp to a team of agents run from Slack, each with the config and commands it takes."
order: 5
---

Each setup below uses only what ships in the binary. Start from a working install ([Quickstart](quickstart.md)), then add the pieces shown. Keys always go in `$HABAKKUK_HOME/.env`, never in `config.yaml`.

## A personal assistant on WhatsApp

An agent with its own chat in your WhatsApp, with no phone number to give up and no public URL.

1. In WhatsApp, open Settings > Agents > Create agent, and send it "hi".
2. Open its chat, then Chat info > API key, and copy the key.
3. Run the wizard and pick **WhatsApp agent**:

```bash
habakkuk setup
habakkuk setup service      # Linux and macOS: write a systemd or launchd file, and print how to install it
```

The channel it writes looks like this:

```yaml
channels:
  wagent:
    type: whatsappagent
    token_env: WHATSAPP_AGENT_KEY
```

Only you, the agent's creator, can write to it. See [WhatsApp](../channels/whatsapp.md#whatsapp-agents).

## A daily briefing

A message every weekday morning with what you asked for. Set your time zone, make sure the agent can search the web, and add a schedule.

```yaml
timezone: America/Toronto
search:                     # not needed when the agent's model is on an anthropic provider,
  provider: brave           # which brings its own hosted web search
  key_env: BRAVE_API_KEY
```

Ask in a chat ("every weekday at 7:30, send me the top three stories about renewable energy and today's weather in Toronto"), and the agent creates the schedule with `schedule_create`. Or add it from the terminal:

```bash
habakkuk schedules add -agent habakkuk -name briefing -when "30 7 * * 1-5" \
  -prompt "Search the news and send me the top three stories about renewable energy, then today's weather in Toronto." \
  -to wagent:creator
habakkuk schedules run <id>     # try it now
```

`habakkuk serve` must be running. See [Schedules](../agents/schedules.md) and [Media and web search](../tools/media.md#web-search).

## A team of business agents on WhatsApp, run from Slack

Marketing, sales and support each have their own WhatsApp agent chat. An orchestrator answers you on Slack and delegates to them.

1. Create three WhatsApp agents (Settings > Agents), and send each one a message.
2. Create a Slack app with Socket Mode (see [Slack](../channels/slack.md)).
3. Configure the agents and channels:

```yaml
agents:
  orchestrator:
    model: anthropic/claude-opus-5-5     # the default toolsets include delegate and board
  marketing:
    model: anthropic/claude-sonnet-5-5
    home: wa-marketing:creator
  sales:
    model: anthropic/claude-sonnet-5-5
    home: wa-sales:creator
  support:
    model: anthropic/claude-sonnet-5-5
    home: wa-support:creator
channels:
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN
    bot_tokens_env: [SLACK_BOT_TOKEN]
    agent: orchestrator
  wa-marketing: {type: whatsappagent, token_env: WA_MARKETING_KEY, agent: marketing}
  wa-sales:     {type: whatsappagent, token_env: WA_SALES_KEY, agent: sales}
  wa-support:   {type: whatsappagent, token_env: WA_SUPPORT_KEY, agent: support}
```

Ask the orchestrator on Slack "what's up with the campaign?" and it delegates to marketing. The answer comes back to Slack, and a short report lands in the Marketing chat on WhatsApp, so each agent's chat keeps the history of its own work. See [Multi-agent teams](../agents/multi-agent.md#teams-and-home-chats).

## A research agent

A second agent with a browser and web search, that you reach with `/agent research` or that your main agent delegates to.

```yaml
search:                            # web_search for models without hosted search
  provider: tavily
  key_env: TAVILY_API_KEY
agents:
  research:
    model: deepseek/deepseek-v4-pro
    permission: workspace          # web reads run on their own; commands ask first
    toolsets: [core, memory, browser]
browser:
  mode: launch
  headless: true
```

```bash
habakkuk run -agent research "Compare the three most used open-source CRMs. A table, then a recommendation."
```

On a server, install Chromium first (`apt install chromium` on Debian). For long jobs, put a card on the [board](../agents/board.md) assigned to `research`: the result comes back to the conversation that asked. See [Browser](../tools/browser.md).

## A home-automation agent

Lights, climate and media from a chat, and a turn that starts on its own when the doorbell rings.

```yaml
agents:
  habakkuk:
    toolsets: [core, memory, skills, schedule, homeassistant]
homeassistant:
  url: http://homeassistant.local:8123
  token_env: HA_TOKEN                        # a long-lived access token
  allow_domains: [light, switch, climate, media_player]
  confirm: [lock, alarm_control_panel, cover]  # every call asks you first
  triggers:
    - name: doorbell
      entity_id: binary_sensor.front_doorbell
      to: "on"
      prompt: "Someone rang the doorbell at {{.Time}}. Check the porch camera and tell me who it is."
      deliver: telegram:123456789
```

Then ask "turn off the living room lights" in any chat. See [Integrations: Home Assistant](../tools/integrations.md#home-assistant).

## A meeting note-taker

The agent joins Google Meet, Zoom or Teams calls as a named guest, reads the live captions, and sends notes with decisions and action items.

```yaml
agents:
  habakkuk:
    toolsets: [core, memory, skills, schedule, meeting]
meeting:
  name: Habakkuk (notetaker)
```

Ask "join https://meet.google.com/abc-defg-hij and note the decisions", or "every weekday at 9:55, join my standup at <link>", which becomes a schedule. See [Meetings](../tools/meetings.md).

## A coding agent in a sandbox, in your editor

An agent whose commands run in a Docker container, with workspace snapshots before every change, used from an editor that speaks the Agent Client Protocol.

```yaml
agents:
  coder:
    model: anthropic/claude-opus-5-5
    workspace: ~/src/myproject
    sandbox: docker
    docker: {image: "golang:1.27", network: false, cpus: "2", memory: 2g}
```

```bash
habakkuk acp -agent coder        # configure your editor to run this as its agent
habakkuk checkpoints -agent coder
habakkuk rollback -agent coder <id>
```

`/undo` in a chat puts back the files the last turn changed. See [Files and shell](../tools/files-and-shell.md) and [Integrations: code editors](../tools/integrations.md#code-editors-acp).

## A watcher that only speaks when something changes

A script schedule runs a command without calling a model, and posts only when the script prints something. It costs nothing while there is nothing to say.

```bash
cat > ~/habakkuk-workspace/check-site.sh <<'EOF'
#!/bin/sh
code=$(curl -s -o /dev/null -w '%{http_code}' https://example.com)
[ "$code" = 200 ] || echo "example.com answered $code"
EOF
chmod +x ~/habakkuk-workspace/check-site.sh

habakkuk schedules add -agent habakkuk -name site-check -when "@every 5m" \
  -run ./check-site.sh -on-output deliver -timeout 30s -to telegram:123456789
```

The script runs in the agent's workspace (here `~/habakkuk-workspace`, as `habakkuk setup` writes it); `-workdir DIR` picks another folder. With `-on-output agent -prompt "..."`, the output starts a turn instead, so the agent can look into it. Only you can add script schedules, from the CLI. See [Script schedules](../agents/schedules.md#script-schedules).
