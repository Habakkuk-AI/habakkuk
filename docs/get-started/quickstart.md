---
title: Quickstart
description: "Install Habakkuk, run the setup wizard, get your first reply in about a minute, and learn the everyday commands."
order: 1
---

This page takes you from nothing to an agent that answers you, in three steps: install, run setup, say hi. It takes a few minutes. The rest of the page covers the commands you'll use every day.

## 1. Install

```bash tab="macOS / Linux"
curl -fsSL https://habakkuk.ai/install.sh | sh
```

```powershell tab="Windows"
irm https://habakkuk.ai/install.ps1 | iex
```

[Installation](installation.md) has the other ways (a release archive, `go install`, the container image) and the options.

## 2. Run the setup wizard

Not sure what to run? Type `hk` (or `habakkuk`) on its own: on a new computer it prints one short screen that ends with the command below.

```bash
habakkuk setup
```

It takes about a minute from download to your agent's first reply, and there's no YAML to edit. The wizard has three steps.

1. **Pick a model.** It finds API keys already in your environment or `.env` (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `DEEPSEEK_API_KEY`, `OPENROUTER_API_KEY` and the other [presets](../models/index.md#provider-directory)), ChatGPT or GitHub Copilot sign-ins, and model servers running on this computer (Ollama, LM Studio, vLLM, llama.cpp, LiteLLM). It recommends the best one, paired with the same provider's cheaper model for quick messages ([routing](../models/routing.md)). Any provider works; nothing assumes Claude. You can also paste a key (it is not shown as you type) or sign in. One tiny live call checks it, with a timeout and a plain error if it fails.
2. **Pick where to talk.** The recommended one is a **WhatsApp agent**: in WhatsApp, open Settings > Agents > Create agent and send it "hi". Then in its chat open Chat info > API key, and paste the key. Setup checks the key with one poll that skips no messages, finds your "hi" and lets only you in. It can also connect a Telegram bot (token checked with `getMe`), WhatsApp as a linked device (it runs the QR or pairing-code link), the web chat on this computer, Slack or Discord (tokens checked), or just the terminal.
3. **Names.** What your agent is called (default Habakkuk) and, if you like, what it should call you. On your first chat it still introduces itself, under that name.

It then writes a short, commented `config.yaml` holding what you chose plus two starter defaults: the [dashboard](../interfaces/dashboard.md) on this computer only (`http.listen` on loopback, the next free port if 8484 is taken) and [memory review](../agents/memory.md) on, so your agent keeps what matters from each chat without you asking. Keys go to `.env`, readable only by you, never to the config. It runs `habakkuk doctor`, then ends with a summary like this one:

```text
Nova is ready. Setup took 2.1s.

  Model      anthropic/claude-opus-5-5, and anthropic/claude-haiku-5-5 for quick messages (cheaper)
  Talk       your WhatsApp agent; only you can message it
  Memory     on: Nova keeps what matters from each chat
  Dashboard  http://127.0.0.1:8484/app/ while habakkuk serve runs
  Settings   ~/.habakkuk/config.yaml (keys in .env beside it)

Try these first:
  1. Your agent answers the message you sent it in WhatsApp.
  2. Get a morning brief: "Every weekday at 8, send me a short brief of my day."
  3. Teach it about you: "Remember that I like short, direct answers."

Schedules, channels and memory reviews run while habakkuk serve does.
```

Then it offers to keep your agent running. On macOS and Linux, say yes and it installs a launchd agent or a systemd user unit, starts it, and waits until `habakkuk serve` is up, so it keeps answering after you close the terminal and after a restart. Say no and it offers to run `habakkuk serve` in this terminal instead. With `-y` it never installs or starts anything unless you pass `-service` (which writes the file and prints the commands) or `-serve`, and it never opens a chat.

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

`habakkuk chat` opens the full-screen terminal UI on a terminal (`-plain` keeps a line-by-line chat). An empty chat suggests things to try. The first conversation opens with a welcome in which the agent introduces itself, suggests three things you can ask for right away (a morning brief first, when it can schedule tasks) and asks what to call you; see [Agents and identity](../agents/agents.md#the-first-chat).

The dashboard's Overview page has a **Get started** panel with the same first steps, and every empty page (Chat, Schedules, Memory, Skills, the Board) says what belongs there and offers a one-click message that fills it.

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
habakkuk status                                      # serve uptime, channels, queue, next schedule, today's cost
habakkuk doctor                                      # check everything; each problem comes with its fix
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

> [!TIP]
> Coming from another agent? On a new install `habakkuk setup` offers to import it, or run `habakkuk migrate --dry-run` to see what would move. See [Move from another agent](migrate.md).

## Next steps

<!-- cards -->
- [Configuration](configuration.md): how `config.yaml` is laid out and where keys go.
- [Channels](../channels/index.md): connect Telegram, Slack, Discord and the rest.
- [Use cases](use-cases.md): concrete setups to copy.
