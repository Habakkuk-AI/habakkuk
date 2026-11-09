---
title: Quick start
description: Run the setup wizard, get your first WhatsApp reply in about a minute, and learn the everyday commands.
order: 2
---

## Set up and chat

```bash
habakkuk setup
```

About a minute from download to your agent's first WhatsApp reply, and no YAML to edit. The wizard has three steps:

1. **Pick a model.** It finds API keys already in your environment or `.env` (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `DEEPSEEK_API_KEY`, `OPENROUTER_API_KEY` and the other [presets](../guides/models.md#providers)), ChatGPT or GitHub Copilot sign-ins, and model servers running on this computer (Ollama, LM Studio, vLLM, llama.cpp, LiteLLM), and recommends the best one. Or paste a key (it is not shown as you type), or sign in. One tiny live call checks it, with a timeout and a plain error if it fails.
2. **Pick where to talk.** The recommended one is a **WhatsApp agent**: in WhatsApp, open Settings > Agents > Create agent and send it "hi"; then in its chat open Chat info > API key, and paste the key. Setup checks the key with one poll that skips no messages, finds your "hi" and lets only you in. It can also connect a Telegram bot (token checked with getMe), WhatsApp as a linked device (runs the QR or pairing-code link), the web chat on this computer, Slack or Discord (tokens checked), or just the terminal.
3. **Names.** What your agent is called (default Habakkuk) and, if you like, what it should call you. On your first chat it still introduces itself, under that name.

It then writes a short, commented `config.yaml` holding only what you chose (keys go to `.env`, readable only by you, never to the config), runs `habakkuk doctor`, prints the next step (for example, *send a message to your agent in WhatsApp*), and offers to start `habakkuk serve` now. On Linux and macOS it can instead write a systemd user unit or a launchd agent and show the command that installs it; it never installs one by itself.

Run it again to change something: it edits `config.yaml` in place, keeps everything you did not change (comments too), and saves the previous file as `config.yaml.bak`. Or do one piece at a time:

```bash
habakkuk setup model                   # another model or provider
habakkuk setup channel whatsappagent   # add a channel: whatsappagent, telegram, whatsapp, web, slack, discord
habakkuk setup name                    # rename your agent, or tell it your name
habakkuk setup service                 # write the background service file and show how to enable it
```

In scripts and CI, `-y` never prompts. Answers come from flags; keys and tokens come only from the environment or `.env`, never from flags (`WHATSAPP_AGENT_KEY`, `TELEGRAM_BOT_TOKEN`, `SLACK_APP_TOKEN` and `SLACK_BOT_TOKEN`, `DISCORD_BOT_TOKEN`, and the provider's key):

```bash
ANTHROPIC_API_KEY=... WHATSAPP_AGENT_KEY=... habakkuk setup -y -channel whatsappagent -name Nova -user Ada
habakkuk setup -y -provider ollama -model llama3.3 -channel web -serve
habakkuk setup -h                      # every flag: -provider, -model, -base-url, -key-env, -channel, -allow, -phone,
                                       # -name, -user, -workspace, -agent, -skip-verify, -timeout, -serve, -service
```

Then talk to it in the channel you picked, or with `habakkuk chat`.

Or by hand:

```bash
habakkuk init                      # writes ~/.habakkuk/config.yaml and agents/habakkuk/SOUL.md
echo 'ANTHROPIC_API_KEY=...' >> ~/.habakkuk/.env
habakkuk doctor
habakkuk chat
```

Other commands:

```bash
habakkuk run "summarise the files in my workspace"   # one turn, print the answer
habakkuk serve                                       # channels and workers: resume interrupted turns, apply retention
habakkuk channels                                    # channel health
habakkuk schedules                                   # scheduled tasks (agents create them with schedule_create)
habakkuk mcp                                         # MCP servers: status and tools
habakkuk mcp catalog search                          # well-known MCP servers; habakkuk mcp add NAME configures one
habakkuk pair ABC123                                 # approve someone who messaged the bot
habakkuk whatsapp link -phone +15551234567           # link WhatsApp with a code typed on the phone
habakkuk sessions                                    # recent conversations
habakkuk show <session>                              # print a transcript
habakkuk memory                                      # an agent's memories, newest first
habakkuk memory search coffee                        # search them
habakkuk memory show <slug>                          # one note, with where it came from
habakkuk memory forget <slug>                        # move a note to the memory trash
habakkuk memory trust <slug> [yes|no]                # vouch for a note written from outside content
habakkuk memory profile                              # the agent's profile of you (USER.md)
habakkuk memory log                                  # what the background review changed
habakkuk skills                                      # skills an agent can use
habakkuk skills install github:owner/repo/skill      # install a skill (also URLs, paths, registry names)
habakkuk plugins install ./my-plugin                 # MCP servers, skills, hooks and commands in one package
habakkuk checkpoints                                 # workspace snapshots taken before turns changed files
habakkuk rollback <id> [path]                        # restore the workspace, or one file, to a snapshot
habakkuk backup -o habakkuk.tar.gz                     # config, agents, skills and database snapshots (-env adds secrets)
habakkuk restore habakkuk.tar.gz                       # unpack a backup into $HABAKKUK_HOME
habakkuk update -check                               # is there a newer release? (habakkuk update installs it)
habakkuk models                                      # providers and the models they serve
habakkuk auth login chatgpt                          # sign in with a subscription instead of a key
habakkuk usage -days 7                               # tokens and cost by model and agent
```

In a chat, `/help` lists the commands: `/new`, `/stop`, `/status`, `/compact`, `/retry`, `/btw <question>` (a side question that stays out of the conversation), `/steer <text>` (guidance for the task in progress), `/queue <text>`, `/think low|medium|high|xhigh|max`, `/personality <name>`, `/goal <text>` (keep working until done), `/export`, `/whoami`, `/agents` and `/agent <name>`.

`HABAKKUK_HOME` overrides `~/.habakkuk`.

Or by hand:

```bash
habakkuk init                      # writes ~/.habakkuk/config.yaml and agents/habakkuk/SOUL.md
echo 'ANTHROPIC_API_KEY=...' >> ~/.habakkuk/.env
habakkuk doctor
habakkuk chat
```

`habakkuk chat` opens the full-screen terminal UI on a terminal (`-plain` keeps a line-by-line chat). The first conversation opens with a welcome in which the agent introduces itself and asks what to call you; see [Agents, identity and the first chat](../guides/agents.md).

`HABAKKUK_HOME` overrides `~/.habakkuk`.

## Keep it running

`habakkuk chat` runs turns while it is open. To reach the agent from chat apps, run schedules and the board, and serve the dashboard, run:

```bash
habakkuk serve                                       # channels and workers: resume interrupted turns, apply retention
```

See [Channels](../guides/channels.md) to connect a chat app and [Deployment](../guides/deployment.md) to run `serve` as a service.

## Other commands

```bash
habakkuk run "summarise the files in my workspace"   # one turn, print the answer
habakkuk channels                                    # channel health
habakkuk schedules                                   # scheduled tasks (agents create them with schedule_create)
habakkuk mcp                                         # MCP servers: status and tools
habakkuk mcp catalog search                          # well-known MCP servers; habakkuk mcp add NAME configures one
habakkuk pair ABC123                                 # approve someone who messaged the bot
habakkuk whatsapp link -phone +15551234567           # link WhatsApp with a code typed on the phone
habakkuk sessions                                    # recent conversations
habakkuk show <session>                              # print a transcript
habakkuk memory                                      # an agent's memories, newest first
habakkuk memory search coffee                        # search them
habakkuk memory show <slug>                          # one note, with where it came from
habakkuk memory forget <slug>                        # move a note to the memory trash
habakkuk memory trust <slug> [yes|no]                # vouch for a note written from outside content
habakkuk memory profile                              # the agent's profile of you (USER.md)
habakkuk memory log                                  # what the background review changed
habakkuk skills                                      # skills an agent can use
habakkuk skills install github:owner/repo/skill      # install a skill (also URLs, paths, registry names)
habakkuk plugins install ./my-plugin                 # MCP servers, skills, hooks and commands in one package
habakkuk checkpoints                                 # workspace snapshots taken before turns changed files
habakkuk rollback <id> [path]                        # restore the workspace, or one file, to a snapshot
habakkuk backup -o habakkuk.tar.gz                   # config, agents, skills and database snapshots (-env adds secrets)
habakkuk restore habakkuk.tar.gz                     # unpack a backup into $HABAKKUK_HOME
habakkuk update -check                               # is there a newer release? (habakkuk update installs it)
habakkuk models                                      # providers and the models they serve
habakkuk auth login chatgpt                          # sign in with a subscription instead of a key
habakkuk usage -days 7                               # tokens and cost by model and agent
```

The [CLI reference](../reference/cli.md) lists every command and flag.

## Chat commands

In a chat, `/help` lists the commands: `/new`, `/stop`, `/status`, `/compact`, `/retry`, `/btw <question>` (a side question that stays out of the conversation), `/steer <text>` (guidance for the task in progress), `/queue <text>`, `/think low|medium|high|xhigh|max`, `/personality <name>`, `/goal <text>` (keep working until done), `/export`, `/whoami`, `/agents` and `/agent <name>`. The [CLI reference](../reference/cli.md#chat-commands) describes them all.

Next: [Configuration](configuration.md).
