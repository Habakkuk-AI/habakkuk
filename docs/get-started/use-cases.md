---
title: Use cases
description: "Eight concrete setups, from a personal assistant on WhatsApp to a team of agents run from Slack, each with the config and commands it takes."
order: 5
---

Here are eight setups you can copy, each with the config and commands it takes. Every one uses only what ships in the binary. Start from a working install (see the [Quickstart](quickstart.md)), then add the pieces shown. Keys always go in `$HABAKKUK_HOME/.env` and stay out of `config.yaml`.

## A personal assistant on WhatsApp

This gives you an agent with its own chat in your WhatsApp. You don't give up a phone number, and you don't need a public URL.

1. In WhatsApp, open Settings > Agents > Create agent. Send the new agent "hi".
2. Open its chat, go to Chat info > API key, and copy the key.
3. Run the setup wizard and pick **WhatsApp agent**:

```bash
hk setup
hk setup service            # Linux and macOS: write a systemd or launchd file, and print how to install it
```

The wizard writes a channel that looks like this:

```yaml
channels:
  wagent:
    type: whatsappagent
    token_env: WHATSAPP_AGENT_KEY
```

You created the agent, so only you can write to it. See [WhatsApp](../channels/whatsapp.md#whatsapp-agents).

## A daily briefing

Your agent sends you a message every weekday morning with what you asked for. To get there, set your time zone, make sure the agent can search the web, and add a schedule.

```yaml
timezone: America/Toronto
search:                     # not needed when the agent's model is on an anthropic provider,
  provider: brave           # which brings its own hosted web search
  key_env: BRAVE_API_KEY
```

One way to add the schedule is to ask in a chat: "every weekday at 7:30, send me the top three stories about renewable energy and today's weather in Toronto". The agent then creates it with `schedule_create`. The other way is to add it from the terminal:

```bash
hk schedules add -agent habakkuk -name briefing -when "30 7 * * 1-5" \
  -prompt "Search the news and send me the top three stories about renewable energy, then today's weather in Toronto." \
  -to wagent:creator
hk schedules run <id>           # try it now
```

For this to work, `hk serve` has to be running. See [Schedules](../agents/schedules.md) and [Media and web search](../tools/media.md#web-search).

## A team of business agents on WhatsApp, run from Slack

In this setup, marketing, sales and support each have their own WhatsApp agent chat. A fourth agent, the orchestrator, answers you on Slack and delegates to them.

1. Create three WhatsApp agents under Settings > Agents, and send each one a message.
2. Set up a Slack app with Socket Mode (see [Slack](../channels/slack.md)).
3. Add the agents and channels to your config:

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

Now ask the orchestrator on Slack "what's up with the campaign?". It delegates the question to marketing. The answer comes back to Slack, and a short report lands in the Marketing chat on WhatsApp. That way, each agent's chat keeps the history of its own work. See [Multi-agent teams](../agents/multi-agent.md#teams-and-home-chats).

## A research agent

This is a second agent with a browser and web search. You reach it with `/agent research`, or your main agent delegates to it.

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
hk run -agent research "Compare the three most used open-source CRMs. A table, then a recommendation."
```

On a server, install Chromium first. On Debian, that's `apt install chromium`. For long jobs, put a card on the [board](../agents/board.md) and assign it to `research`. The result comes back to the conversation that asked. See [Browser](../tools/browser.md).

## A home-automation agent

With this setup, you control lights, climate and media from a chat. A turn also starts on its own when the doorbell rings.

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

Once that's in your config, ask "turn off the living room lights" in any chat. See [Integrations: Home Assistant](../tools/integrations.md#home-assistant).

## A meeting note-taker

Your agent can join Google Meet, Zoom or Teams calls as a named guest. It reads the live captions and sends notes with the decisions and action items.

```yaml
agents:
  habakkuk:
    toolsets: [core, memory, skills, schedule, meeting]
meeting:
  name: Habakkuk (notetaker)
```

To use it, ask "join https://meet.google.com/abc-defg-hij and note the decisions". You can also ask "every weekday at 9:55, join my standup at <link>", and that becomes a schedule. See [Meetings](../tools/meetings.md).

## A coding agent in a sandbox, in your editor

This agent runs its commands in a Docker container and takes a snapshot of the workspace before every change. You use it from an editor that speaks the Agent Client Protocol.

```yaml
agents:
  coder:
    model: anthropic/claude-opus-5-5
    workspace: ~/src/myproject
    sandbox: docker
    docker: {image: "golang:1.27", network: false, cpus: "2", memory: 2g}
```

```bash
hk acp -agent coder              # configure your editor to run this as its agent
hk checkpoints -agent coder
hk rollback -agent coder <id>
```

In a chat, send `/undo` to put back the files the last turn changed. See [Files and shell](../tools/files-and-shell.md) and [Integrations: code editors](../tools/integrations.md#code-editors-acp).

## A watcher that only speaks when something changes

A script schedule runs a command without calling a model. It posts only when the script prints something, so it costs nothing while there's nothing to say.

```bash
cat > ~/habakkuk-workspace/check-site.sh <<'EOF'
#!/bin/sh
code=$(curl -s -o /dev/null -w '%{http_code}' https://example.com)
[ "$code" = 200 ] || echo "example.com answered $code"
EOF
chmod +x ~/habakkuk-workspace/check-site.sh

hk schedules add -agent habakkuk -name site-check -when "@every 5m" \
  -run ./check-site.sh -on-output deliver -timeout 30s -to telegram:123456789
```

The script runs in the agent's workspace. Here that's `~/habakkuk-workspace`, as `hk setup` writes it. To run it in another folder, pass `-workdir DIR`. If you use `-on-output agent -prompt "..."`, the output starts a turn instead, so the agent can look into it. Script schedules can only be added by you, from the CLI. See [Script schedules](../agents/schedules.md#script-schedules).
