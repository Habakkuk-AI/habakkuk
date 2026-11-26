---
title: Agents and identity
description: "Define an agent, give it a personality and standing instructions, switch personas, and see how the first-chat welcome lets you name it."
order: 1
---

An agent is defined in your config, and a few files shape how it talks. Come here when you set up a new agent, or when you want to change one's settings, personality or language.

## Defining an agent

```yaml
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
    fallbacks: [deepseek/deepseek-v4-pro]
    effort: high                    # low | medium | high | xhigh | max
    permission: workspace           # read-only | ask | workspace (default) | full
    approvals: human                # human (default) | reviewer | reviewer_then_human
    workspace: ~/habakkuk-workspace
    toolsets: [core, memory, skills, schedule, delegate, board]   # the default when not set
```

| Setting | Meaning |
|---|---|
| `description` | What the agent does, in a sentence, for people. The dashboard shows it; it is never sent to the model |
| `model`, `fallbacks` | `<provider>/<model id>`, then models tried in order when one fails in a way another can fix. See [Model providers](../models/providers.md) |
| `fast_model`, `routing`, `escalate_after` | A cheaper model for quick messages and chores, and how turns are split between it and `model`. See [Routing](../models/routing.md) |
| `effort` | The reasoning effort sent with each request; `/think` overrides it per session |
| `max_tokens`, `max_iterations` | Output tokens per model call (default 64000) and model calls per turn (default 200) |
| `loop_guard` | When to warn and then stop an agent that keeps repeating the same step. See [When an agent goes round in circles](#when-an-agent-goes-round-in-circles) |
| `permission`, `approvals`, `reviewer_model` | What the agent may do without asking, and who decides when it must ask. See [Permissions and approvals](approvals.md) |
| `trust`, `channel_allow`, `allow_tainted_exec` | Lift the extra checks on chat-driven and tainted turns. See [Security](../deploy/security.md) |
| `workspace` | The folder its file tools and commands work in (default `$HABAKKUK_HOME/agents/<name>/workspace`) |
| `project_files` | Read the project's `AGENTS.md`, `CLAUDE.md` and `.cursorrules` into new sessions: `on` (default) or `off`. They're followed only after you trust their exact content with `hk projects trust`. See [Project instruction files](#project-instruction-files) |
| `toolsets` | The tools it gets. Without the setting: `core`, `memory`, `skills`, `schedule`, `delegate` and `board`. A list replaces the default. See [Tools](../tools/index.md) |
| `web` | Use the provider's hosted web search and fetch where the model supports it (default `true`) |
| `compact_at` | Summarize older messages once a prompt reaches this many tokens (default 600000 on `anthropic` providers, 150000 otherwise; `-1` turns it off) |
| `title_model` | The model that writes session titles (default `fast_model`, else the agent's chain; `off` disables titles) |
| `sandbox`, `checkpoints`, `tool_search_over` | Where commands run, workspace snapshots, and when to switch to tool search. See [Files and shell](../tools/files-and-shell.md) |
| `home`, `report_home` | The agent's own chat, where it reports work that started elsewhere. See [Multi-agent teams](multi-agent.md#teams-and-home-chats) |
| `budget` | Spending caps for this agent. See [Usage and limits](../models/usage-and-limits.md#spending-limits) |

## When an agent goes round in circles

Sometimes a model gets stuck. It runs the same command, gets the same error and tries again, and every try costs you money. `max_iterations` would end that eventually, but only after 200 model calls. The loop guard catches it much sooner.

It watches each round of tool calls for two things:

- **Repeats.** The same call, with the same tool and the same arguments, failing with the same error, and tried again straight away: in the very next round, with nothing that worked in between, within 20 seconds. A call whose tool actually ran for 2 seconds or more before it failed isn't a repeat (time spent in hooks or waiting for an approval doesn't count), such as `sleep 10 && curl localhost:3000`. That's how an agent waits for a server. Only time spent counts: a command that fails at once is a repeat whatever it says, such as `timeout 120 ...` or `curl --retry 5 ...`.
- **Idle rounds.** Rounds in a row where every call is one the agent already made in this turn and the results come back the same. Nothing new is learned. A round where every call took 2 seconds or more, such as a `sleep 30`, doesn't count. So an agent that checks a server, sleeps and checks again is stopped at its 13th check: the first check is new, and the 12 after it are idle rounds.

The first time a count reaches its warn level, the agent is told plainly that it's repeating itself and should change its approach or stop and tell you what's blocking it. If it keeps going and reaches the stop level, the turn ends and you get a message saying why, such as "I stopped working on this because I was going round in circles: I tried read_file 5 times and it failed the same way each time". Reply to tell it how to go on.

```yaml
agents:
  habakkuk:
    loop_guard:
      repeat_warn: 3    # the same failing call, 3 times (default)
      repeat_stop: 5    # default 5
      idle_warn: 8      # rounds in a row with nothing new (default 8)
      idle_stop: 12     # default 12
```

Leave out a key to keep its default. Set it to `-1` to turn that check off. A warn level must be below its stop level. The idle levels are higher on purpose: waiting on a slow job by checking it again and again is a fair reason to repeat a call. A run nobody was watching, such as a schedule, that the guard stopped counts as failed, and its chat and home chat get the message.

Each warning and stop fires the `loop_guard` [hook](approvals.md) and counts in the `habakkuk_loop_guard_total` metric. See [Observability](../deploy/observability.md). A turn that's resumed after a restart picks up its counts from its own transcript, so a restart doesn't reset them.

## Personality and instructions

Each agent keeps its files in `$HABAKKUK_HOME/agents/<name>/`:

| File | What it is |
|---|---|
| `SOUL.md` | The agent's personality. `hk init` writes one for the default agent |
| `AGENTS.md` | Standing instructions |
| `IDENTITY.md` | The name, emoji and manner the user chose in chat |
| `USER.md` | The agent's profile of the user. See [Memory](memory.md#the-user-profile) |

Habakkuk reads these files when a session starts. Your edits reach every new session (`/new`), and you don't need to restart. A session that's already open keeps the prompt it started with, so the provider's prompt cache stays valid.

### Project instruction files

Point an agent's `workspace` at a code project and it picks up the instructions that project keeps for coding agents. When a session starts, Habakkuk looks for `AGENTS.md`, `CLAUDE.md` and `.cursorrules` in the folder the session works in and in each folder above it, up to the root of its git repository. That folder is the agent's workspace, a delegated task's git worktree, or the project an editor opened. The files are read from the repository root down, so the rules closest to the folder come last. Outside a repository, only the folder itself is read.

**Files are untrusted unless their exact content was trusted with `hk projects trust`.** Anyone who can change a project can write these files, so Habakkuk doesn't follow them just because they're there. Not even in the agent's own workspace.

- **To have the agent follow a file**, look at it and trust it. Run `hk projects trust` in the project's folder. It shows exactly the text the agent gets from each file, with any control or invisible characters made visible (`\x1b`, `<U+202E>`). To trust a file, you type back a short random code it prints, such as `K7MX`. A file holding such characters isn't trusted unless you add `-allow-control`. `hk projects trust FILE` asks about one file. It records each file's path and a fingerprint (SHA-256) of its exact content in `$HABAKKUK_HOME/projects/`. A trusted file goes into the session's prompt as instructions.
- **Any change makes a file untrusted again**, whoever made it, even one byte. The agent is told the file changed since you trusted it and how to trust it again. Run `hk projects trust` again after you've read the change.
- **An untrusted file** is given to the agent at the start of the session's first message, as information about the project, not as instructions. And because it's there, **every turn of that session is tainted**, like a turn that read a web page: every command, and every write outside the workspace, needs your approval. See [Security](../deploy/security.md#outside-content-taints-a-turn).
- `hk projects list` shows what you trusted and whether each file changed since. `hk projects untrust` drops a file, or every file under a folder.
- **Who can change that list.** `hk projects trust` always asks, and there's no option to skip the question. On macOS and Linux it asks on your terminal itself (`/dev/tty`), not through standard input, so a pipe or `/dev/null` can't answer it, and it prints nothing unless it's the job in the foreground of that terminal. You confirm by typing back the random code it shows, not by answering yes, so a process that pushes keystrokes into your terminal without seeing it can't confirm for you. On Windows the console is standard input, so it checks that standard input and output are a console. It writes a record only for the exact content it showed you. That's a hurdle for scripts and agents, not a guarantee: a process on your computer that can fake a terminal (with `script`, for example) could still answer it. The agents' file tools, and every tool that saves a file (browser downloads, PDFs and screenshots, generated images and videos, `speak`, files a paired device returns, chat attachments), refuse `$HABAKKUK_HOME/projects/` in every turn and mode, however its path is spelled and through any link. A paired device won't write into any folder laid out as a Habakkuk home with `files.put`, so one running on the same computer as the gateway can't reach its home either. The record counts only while it's private to you: mode 0600 in a 0700 folder you own. If anything else could have written it, nothing is trusted, and `hk doctor --fix` won't tighten it for you: tightening a planted record would make it look like yours. Look at how it got that way, remove the folder, and trust your files again.
- **What full mode can still do.** In a turn with no outside content, an agent in `full` mode can run any shell command without asking. That's what `full` means, and such a command could change the record. If that's a risk for you, use `workspace` mode, where every command needs approval: yours, or with `approvals: reviewer`, a reviewer model's. Or use `ask` mode.
- **Schedules and board cards.** A schedule or board card made in a session with an untrusted project file is tainted, even when you typed the request yourself, and so is every one of its runs. A run's shell commands and writes then need a person whatever the approval mode: the run asks in the chat the schedule or card reports to, and with no chat the call is denied. Saved approval rules never answer for it. A tainted turn can pause a schedule but can't resume a paused one: you resume it with `hk schedules resume ID`, from the dashboard, or by asking in a clean message. See [Approvals in scheduled runs](approvals.md#approvals-in-scheduled-runs).
- **Same file, any spelling.** A file is matched as a file, not by how its path is written: another letter case or a link to it still matches. The same file in a git worktree of the same repository, such as the one a delegated task works in, matches too while its content is the same. Only a real worktree counts: one whose entry in the repository points back to it. A folder with a hand-written `.git` file naming your repository gets no trust from it. `hk projects list PATH` shows what an agent working in `PATH` really gets, file by file.

How they're read:

- **Size.** Each file is cut at 16 KB, and all of them together stop at 32 KB. Files past the limit are left out. A file larger than 1 MB can't be trusted.
- **What was read.** The agent is told every file that was read, cut or left out, and the log has a `project instruction files read` line with the trusted and untrusted paths.
- **Only plain files.** A `CLAUDE.md` that links to `AGENTS.md` is read once. A link to anything that isn't another of these files inside the project, such as `.env` or a key, is left out, and so is anything that isn't a regular file.
- **Never your home folder.** Your home folder, `$HABAKKUK_HOME` and the agent's own folder are never treated as a project, even if you keep them in git, and the search never climbs past them. So your dotfiles stay out.
- **When edits show up.** Like the files above, they're read when a session starts. Send `/new` after you change one.

```yaml
agents:
  coder:
    workspace: ~/code/bramblecrest-bakery
    project_files: on   # on (default) | off
```

Use `off` to stop reading them.

What this doesn't cover:

- **Instructions in other files.** A project's `README.md`, docs or code comments aren't read by Habakkuk, but the agent may open them while it works. Like any file it reads, they don't change what needs your approval.
- **Files a clean turn writes.** In a turn with no outside content in it, the agent's own shell and file tools can still change these files when you let them. The change then isn't trusted: the fingerprint no longer matches, so the file stays untrusted until you trust it again. A turn that has read outside content, or a message from someone other than you, can't write them with the file tools at all.

### Personas

Send `/personality NAME` in a chat to add a persona to the end of the session's prompt. The built-in personas are `concise`, `warm`, `formal`, `technical` and `playful`. You can also write your own as `$HABAKKUK_HOME/personalities/NAME.md`, which is read on every turn. `/personality off` drops the persona, and `/new` starts a session without it.

## The first chat

The first one-to-one conversation an agent has with you opens with a welcome. That conversation can be a direct message on any channel, `hk chat` or the TUI. The agent greets you and introduces itself. It suggests three things to try right away, each written as a message you could send. It picks them from the tools it actually has, and puts a morning brief first when it can schedule tasks. Then it asks three questions: what to call you, whether it should keep its name and how it should talk, and where you are, for time zones. If your first message asks for something real, the agent answers that first.

Your answers go to `agents/<name>/IDENTITY.md` (the name, emoji and manner, written through the `set_identity` tool) and to the user profile, `USER.md`. You can change the identity later too, by saying something like "call yourself Jarvis". The prompt states the identity after `SOUL.md`. That way a soul you wrote by hand keeps its place, and the name you chose wins.

> [!NOTE]
> Habakkuk refuses `set_identity` in a turn that has read outside content, because the identity goes into every prompt.

The welcome runs once per agent. Habakkuk marks it as done with an `.onboarding` file. So if two chats open at the same moment, you get one welcome. And if you ignore the questions, the agent doesn't ask again. An agent that already has an identity or a user profile skips the welcome. An agent moved over from another setup is one example. Automated sessions, such as schedules, webhooks and the board, never get it. On Telegram, `/start` reaches the agent for as long as it hasn't met you. After that, it shows the command list.

## Languages

Habakkuk tells the model to answer in the user's language. Habakkuk's own text is handled separately. That text is the command replies, approval prompts and pairing notices. It comes from catalogs for English, French, Spanish, Portuguese, German, Yoruba, Arabic, Chinese (Simplified), Japanese and Hindi. The non-English catalogs are machine-translated. Set `language:` in the config to choose the default. A channel's own `language:` overrides it.

```yaml
language: fr
channels:
  telegram:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN
    language: es        # Habakkuk's own messages on this channel
```
