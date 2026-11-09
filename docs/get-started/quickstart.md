---
title: Quickstart
description: "Install Habakkuk, run the setup wizard, get your first reply in about a minute, and learn the everyday commands."
order: 1
---

## 1. Install

```bash tab="macOS / Linux"
curl -fsSL https://habakkuk.ai/install.sh | sh
```

```powershell tab="Windows"
irm https://habakkuk.ai/install.ps1 | iex
```

[Installation](installation.md) has the other ways (a release archive, `go install`, the container image) and the options.

## 2. Run the setup wizard

```bash
habakkuk setup
```

About a minute from download to your agent's first reply, and no YAML to edit. The wizard has three steps.

1. **Pick a model.** It finds API keys already in your environment or `.env` (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `DEEPSEEK_API_KEY`, `OPENROUTER_API_KEY` and the other [presets](../models/index.md#provider-directory)), ChatGPT or GitHub Copilot sign-ins, and model servers running on this computer (Ollama, LM Studio, vLLM, llama.cpp, LiteLLM). It recommends the best one, paired with the same provider's cheaper model for quick messages ([routing](../models/routing.md)). Any provider works; nothing assumes Claude. You can also paste a key (it is not shown as you type) or sign in. One tiny live call checks it, with a timeout and a plain error if it fails.
2. **Pick where to talk.** The recommended one is a **WhatsApp agent**: in WhatsApp, open Settings > Agents > Create agent and send it "hi". Then in its chat open Chat info > API key, and paste the key. Setup checks the key with one poll that skips no messages, finds your "hi" and lets only you in. It can also connect a Telegram bot (token checked with `getMe`), WhatsApp as a linked device (it runs the QR or pairing-code link), the web chat on this computer, Slack or Discord (tokens checked), or just the terminal.
3. **Names.** What your agent is called (default Habakkuk) and, if you like, what it should call you. On your first chat it still introduces itself, under that name.

It then writes a short, commented `config.yaml` holding only what you chose. Keys go to `.env`, readable only by you, never to the config. It runs `habakkuk doctor`, prints the next step (for example, *send a message to your agent in WhatsApp*), and offers to start `habakkuk serve` now. On Linux and macOS it can instead write a systemd user unit or a launchd agent and show the command that installs it; it never installs one by itself.

> [!TIP]
> Run `habakkuk setup` again to change something. It edits `config.yaml` in place, keeps everything you did not change (comments too), and saves the previous file as `config.yaml.bak`.

Or change one piece at a time:

```bash
habakkuk setup model                   # another model or provider
habakkuk setup channel whatsappagent   # add a channel: whatsappagent, telegram, whatsapp, web, slack, discord
habakkuk setup name                    # rename your agent, or tell it your name
habakkuk setup service                 # write the background service file and show how to enable it
```

### Without prompts

In scripts and CI, `-y` never prompts. Answers come from flags. Keys and tokens come only from the environment or `.env`, never from flags: `WHATSAPP_AGENT_KEY`, `TELEGRAM_BOT_TOKEN`, `SLACK_APP_TOKEN` and `SLACK_BOT_TOKEN`, `DISCORD_BOT_TOKEN`, and the provider's key.

```bash
ANTHROPIC_API_KEY=... WHATSAPP_AGENT_KEY=... habakkuk setup -y -channel whatsappagent -name Nova -user Ada
habakkuk setup -y -provider ollama -model llama3.3 -channel web -serve
habakkuk setup -h      # every flag: -provider, -model, -base-url, -key-env, -channel, -allow, -phone,
                       # -name, -user, -workspace, -agent, -skip-verify, -timeout, -serve, -service
```

### By hand

```bash tab="macOS / Linux"
habakkuk init                      # writes ~/.habakkuk/config.yaml and agents/habakkuk/SOUL.md
echo 'ANTHROPIC_API_KEY=...' >> ~/.habakkuk/.env
habakkuk doctor
habakkuk chat
```

```powershell tab="Windows"
habakkuk init                      # writes %APPDATA%\Habakkuk\config.yaml and agents\habakkuk\SOUL.md
Add-Content "$env:APPDATA\Habakkuk\.env" 'ANTHROPIC_API_KEY=...'
habakkuk doctor
habakkuk chat
```

`HABAKKUK_HOME` overrides the data folder (`~/.habakkuk`, or `%APPDATA%\Habakkuk` on Windows).

## 3. Talk to it

Talk to it in the channel you picked, or in the terminal:

```bash
habakkuk chat
```

`habakkuk chat` opens the full-screen terminal UI on a terminal (`-plain` keeps a line-by-line chat). The first conversation opens with a welcome in which the agent introduces itself and asks what to call you; see [Agents and identity](../agents/agents.md#the-first-chat).

## 4. Keep it running

`habakkuk chat` runs turns while it is open. To reach the agent from chat apps, run schedules and the board, and serve the dashboard, run:

```bash
habakkuk serve      # channels and workers: resumes interrupted turns, applies retention
```

> [!IMPORTANT]
> Run one `habakkuk serve` per `$HABAKKUK_HOME`. Most channels allow only one program to poll them, so stop any other `serve` first.

See [Channels](../channels/index.md) to connect more chat apps and [Run it on a server](../deploy/server.md) to run `serve` as a service.

## Everyday commands

```bash
habakkuk run "summarise the files in my workspace"   # one turn, print the answer
habakkuk status                                      # serve uptime, channels, queue, today's cost
habakkuk channels                                    # channel health
habakkuk pair ABC123                                 # approve someone who messaged the bot
habakkuk sessions                                    # recent conversations
habakkuk show <session>                              # print a transcript
habakkuk schedules                                   # scheduled tasks (agents create them with schedule_create)
habakkuk memory                                      # an agent's memories, newest first
habakkuk skills                                      # skills an agent can use
habakkuk mcp                                         # MCP servers: status and tools
habakkuk models                                      # providers and the models they serve
habakkuk usage -days 7                               # tokens and cost by model and agent
habakkuk backup -o habakkuk.tar.gz                   # config, agents, skills and database snapshots
habakkuk update -check                               # is there a newer release?
```

The [CLI reference](../reference/cli.md) lists every command and flag.

## Chat commands

In a chat, `/help` lists the commands. The ones you will use most:

| Command | What it does |
|---|---|
| `/new` | Start a fresh conversation |
| `/stop` | Stop the turn in progress |
| `/status` | The agent, session and channel health |
| `/retry` | Run your last message again |
| `/btw <question>` | A side question that stays out of the conversation |
| `/steer <text>` | Guidance for the task in progress |
| `/model [provider/model]` | Show or switch this conversation's model |
| `/goal <text>` | Keep working turn after turn until it is done |
| `/undo` | Put back the files the latest turn changed |

[Sessions and chat commands](../agents/sessions.md) explains them, and the [CLI reference](../reference/cli.md#chat-commands) lists them all.

## Next steps

<!-- cards -->
- [Configuration](configuration.md): how `config.yaml` is laid out and where keys go.
- [Channels](../channels/index.md): connect Telegram, Slack, Discord and the rest.
- [Use cases](use-cases.md): concrete setups to copy.
