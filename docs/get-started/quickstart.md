---
title: Quickstart
description: "Install Habakkuk, run the setup wizard, get your first reply in about a minute, and learn the everyday commands."
order: 1
---

Three steps take you from nothing to an agent that answers you: install, run setup, say hi. It takes a few minutes. The rest of the page covers the commands you'll use every day.

## 1. Install

```bash tab="macOS / Linux"
curl -fsSL https://habakkuk.ai/install.sh | sh
```

```powershell tab="Windows"
irm https://habakkuk.ai/install.ps1 | iex
```

There are other ways to install too: a release archive, `go install` or the container image. [Installation](installation.md) covers them, along with the install options.

## 2. Run the setup wizard

Not sure what to run? Type `hk` (or `habakkuk`) on its own. On a new computer, it prints one short screen that ends with the command below.

```bash
habakkuk setup
```

From download to your agent's first reply takes about a minute, and you don't edit any YAML. The wizard walks you through three steps.

1. **Pick a model.** The wizard looks for what you already have. That's API keys in your environment or `.env` (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `DEEPSEEK_API_KEY`, `OPENROUTER_API_KEY` and the other [presets](../models/index.md#provider-directory)), ChatGPT or GitHub Copilot sign-ins, and model servers running on this computer (Ollama, LM Studio, vLLM, llama.cpp, LiteLLM). It recommends the best one and pairs it with the same provider's cheaper model for quick messages (see [routing](../models/routing.md)). Any provider works, and nothing assumes Claude. You can also paste a key, which isn't shown as you type, or sign in. The wizard checks your choice with one tiny live call. The call has a timeout, and you get a plain error if it fails.
2. **Pick where to talk.** The wizard recommends a **WhatsApp agent**. In WhatsApp, open Settings > Agents > Create agent and send the new agent "hi". Then open Chat info > API key in its chat, and paste the key. Setup checks the key with one poll that skips no messages. It finds your "hi" and lets only you in. Setup can also connect a Telegram bot (it checks the token with `getMe`), WhatsApp as a linked device (it runs the QR or pairing-code link), the web chat on this computer, or Slack or Discord (it checks the tokens). You can also stay with the terminal alone.
3. **Names.** Choose what your agent is called (the default is Habakkuk) and, if you like, what it should call you. The agent still introduces itself in your first chat, under the name you chose.

The wizard then writes a short, commented `config.yaml`. It holds what you chose, plus two starter defaults. The first is the [dashboard](../interfaces/dashboard.md), open to this computer only (`http.listen` on loopback, on the next free port if 8484 is taken). The second is [memory review](../agents/memory.md), switched on so your agent keeps what matters from each chat without you asking. Your keys go to `.env`, which only you can read, and stay out of the config. Last, the wizard runs `habakkuk doctor` and ends with a summary like this one:

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

Next, the wizard offers to keep your agent running. On macOS and Linux, say yes and it installs a launchd agent or a systemd user unit, starts it, and waits until `habakkuk serve` is up. Your agent then keeps answering after you close the terminal and after a restart. If you say no, the wizard offers to run `habakkuk serve` in this terminal instead. With `-y`, the wizard never opens a chat. It also installs and starts nothing unless you pass `-service` (which writes the file and prints the commands) or `-serve`.

> [!TIP]
> To change something later, run `habakkuk setup` again. It edits `config.yaml` in place and keeps everything you didn't change, comments included. The previous file is saved as `config.yaml.bak`.

You can also change one piece at a time:

```bash
habakkuk setup model                   # another model or provider
habakkuk setup channel whatsappagent   # add a channel: whatsappagent, telegram, whatsapp, web, slack, discord
habakkuk setup name                    # rename your agent, or tell it your name
habakkuk setup service                 # write the background service file and show how to enable it
```

### Without prompts

For scripts and CI, pass `-y` and the wizard never prompts. It takes its answers from flags. Keys and tokens are the exception. You can't pass them as flags, so setup reads them only from the environment or `.env`. They are `WHATSAPP_AGENT_KEY`, `TELEGRAM_BOT_TOKEN`, `SLACK_APP_TOKEN` and `SLACK_BOT_TOKEN`, `DISCORD_BOT_TOKEN`, and the provider's key.

```bash
ANTHROPIC_API_KEY=... WHATSAPP_AGENT_KEY=... habakkuk setup -y -channel whatsappagent -name Nova -user Ada
habakkuk setup -y -provider ollama -model llama3.3 -channel web -serve
habakkuk setup -h      # every flag: -provider, -model, -base-url, -key-env, -channel, -allow, -phone,
                       # -name, -user, -workspace, -agent, -skip-verify, -timeout, -serve, -service
```

### By hand

To set things up without the wizard, run these commands:

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

The data folder is `~/.habakkuk`, or `%APPDATA%\Habakkuk` on Windows. Set `HABAKKUK_HOME` to use a different one.

## 3. Talk to it

Message your agent in the channel you picked, or chat with it in the terminal:

```bash
habakkuk chat
```

On a terminal, `habakkuk chat` opens the full-screen terminal UI. Add `-plain` to keep a line-by-line chat. An empty chat suggests things to try. Your first conversation opens with a welcome. In it, the agent introduces itself, suggests three things you can ask for right away (a morning brief comes first, when the agent can schedule tasks) and asks what to call you. See [Agents and identity](../agents/agents.md#the-first-chat).

In the dashboard, the Overview page has a **Get started** panel with the same first steps. Every empty page (Chat, Schedules, Memory, Skills, the Board) tells you what belongs there and offers a one-click message that fills it.

## 4. Keep it running

`habakkuk chat` runs turns while it's open. Reaching the agent from chat apps, running schedules and the board, and serving the dashboard all need this command:

```bash
habakkuk serve      # channels and workers: resumes interrupted turns, applies retention
```

> [!IMPORTANT]
> Run only one `habakkuk serve` for each `$HABAKKUK_HOME`. Most channels let only one program poll them, so stop any other `serve` before you start this one.

To connect more chat apps, see [Channels](../channels/index.md). To run `serve` as a service, see [Run it on a server](../deploy/server.md).

## Everyday commands

Here are the commands for everyday use:

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

For every command and flag, see the [CLI reference](../reference/cli.md).

## Chat commands

Send `/help` in a chat to list the commands. These are the ones you'll use most:

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

[Sessions and chat commands](../agents/sessions.md) explains each one. The [CLI reference](../reference/cli.md#chat-commands) has the full list.

> [!TIP]
> Coming from another agent? On a new install, `habakkuk setup` offers to import it. You can also run `habakkuk migrate --dry-run` to see what would move. See [Move from another agent](migrate.md).

## Next steps

<!-- cards -->
- [Configuration](configuration.md): see how `config.yaml` is laid out and where keys go.
- [Channels](../channels/index.md): connect Telegram, Slack, Discord and the rest.
- [Use cases](use-cases.md): find a concrete setup you can copy.
