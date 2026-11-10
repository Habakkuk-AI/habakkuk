---
title: Security
description: "What an agent may do by default, with approvals, guests, vetted skills, private files, network calls, the local server, the audit and secret managers."
order: 3
---

Habakkuk is safe by default. This page explains what an agent is allowed to do before you change anything, and how to check it on your own machine. Run `hk security` to see what Habakkuk allows there. It prints the effective tool policy of each agent, then an audit with fixes. `hk status` shows the policy too.

```bash
hk security                   # tool policy per agent, then the audit
hk security -fix              # also apply the safe fixes (file permissions)
hk security -network          # every outbound endpoint, and whether your config turns it on
```

## Risky capabilities

Five kinds of tool call can harm the machine Habakkuk runs on. The rest of this page calls them risky capabilities:

| Capability | What it covers |
|---|---|
| `exec` | The shell, and MCP servers marked `risk: exec` |
| `process` | Input to and killing background processes |
| `write` | Files outside the agent's workspace |
| `computer` | Screen, mouse and keyboard, and a paired device's screen, camera and clipboard |
| `browser` | Pages on loopback or private addresses that `browser.allow_private` lets through |

## Chat channels are least-privilege

A turn that comes from a messaging channel needs approval for every risky capability. A messaging channel here means anything except the CLI, the TUI and the local dashboard. The rule applies in every permission mode, `full` included. You give the approval in the chat. If you want your own chat messages to act like the CLI, opt in for each agent:

```yaml
agents:
  habakkuk:
    trust: full               # or lift only some: channel_allow: [write, browser]
```

## Strangers never get them

A sender who is let in by `allow_from: ["*"]`, and not by id or `hk pair`, is a **guest**. A guest's turn is refused every risky capability, with or without approval. Guests can't answer your pending approvals, and they can't use `/remember`, `/forget` or `/agent`. A turn with no sender behind it (a schedule) counts as a guest's turn when it runs on an account that's open to everyone.

There's one exception. A `whatsappagent` channel only ever hears from the person who created the agent, because WhatsApp refuses everyone else. So its sender is always treated as the owner, even with `allow_from: "*"`. Its schedules run as the owner's too, and the audit never counts it as an open channel.

## Outside content taints a turn

A turn is tainted once it has read outside content. Outside content is a web page, a search result, an MCP or other network tool, an email, a webhook or Home Assistant payload, a task another agent sent over A2A, or a device's output. From then on, `exec`, `process` and writes outside the workspace need a human's approval. This holds even in `full` mode and even with `approvals: reviewer`. A model isn't asked, because the same text could steer it. When nobody is attending, these calls are denied.

A tainted turn also can't write trusted memory, change the user profile or set the agent's identity. Its `message_home` sends only a short status. A delegated task inherits its parent's taint and origin.

The taint stays with the turn across a restart. Say a turn reads a web page and is then interrupted by a deploy or a crash. It's still tainted when it resumes. Habakkuk sets the taint again from the turn's own transcript, from any hosted search result and any call to a tool that can bring outside content in.

Setting `allow_tainted_exec: true` on an agent turns this off. The audit flags it.

Saved approval rules don't change this. A rule from **Always allow** or `agents.<name>.allow` never answers for a tainted turn, and a tainted turn is never offered **Always allow**. See [Saved approval rules](#saved-approval-rules).

## Trusted folders for notes

`trusted_write_paths` lets an agent write notes to named folders outside its workspace without approval. That applies from a chat, and after the agent has read outside content. See [how it works](../agents/approvals.md#trusted-folders-for-notes).

This setting really does loosen the rules, so you should know what it gives up. A web page or an email the agent reads can steer what it writes, and with this setting nobody is asked first. Text planted by an attacker can end up in a trusted folder.

These limits keep that from doing more harm:

- The setting covers only the file tools. Commands still need approval, so the agent can't run a planted file without you.
- Scripts, programs, executables and hidden files aren't written there.
- The setting never covers Habakkuk's own folders, so a page can't rewrite the agent's memory, skills, hooks or config this way.
- Symlinks are resolved before the check.

It doesn't protect you against these:

- **Another program that acts on files in that folder.** That could be a build tool, a sync job, a static site generator, or a script of your own that reads `todo.md`. If something runs or loads what's in the folder, don't trust the folder.
- **Wrong or misleading notes.** The agent may write down something a page told it. Keep that in mind when you read a note, as you would with an email.
- **Overwritten notes.** A trusted folder can be edited as well as added to. If the notes matter, keep the folder in version control or a backup.

Use this setting for folders that hold only notes. `hk security` lists each agent's trusted folders. It flags them on an agent that chats or webhooks can reach.

## Saved approval rules

**Always allow** in a chat, and `agents.<name>.allow` in the config, let an agent run one command line, or write to one folder, without asking you each time. See [how it works](../agents/approvals.md#always-allow).

A rule is a standing yes, so it's kept narrow:

- **Your turns only.** Rules answer for a message you sent (in a chat, or typed on this computer) and for the run of a schedule you made. Never for board card runs, sub-agents and their results, notices, the API, MCP clients, editors, A2A tasks, webhooks or guests. Those ask a person.
- **Where you gave it.** A rule saved in a chat applies in that chat (and to its schedules). One saved on this computer applies to turns started here.
- **One line, one folder.** An exec rule is one exact command line, run in one folder exactly. Only quoting may differ. There are no wildcard rules yet.
- **Never in a tainted turn.** Once a turn has read outside content, a rule doesn't answer. A person does.
- **Paused after taint.** Anything tainted that enters a conversation (a tainted turn, a tainted result or schedule run, an A2A task, a message a tainted run delivered to the chat) pauses its rules until your next message there. Work a tainted turn sets up, a schedule or a board card, runs tainted, and so do runs of cards waiting on it.
- **Never past the hardline.** Commands on the blocklist never run, whatever a rule says.
- **Never for the agent's instructions.** Writes to `SOUL.md`, `AGENTS.md`, `IDENTITY.md`, `USER.md`, the saved rules file and its lock always ask, in every mode. Names are compared without regard to case (Unicode folding included). A command that names one, or whose arguments lead to one through a symlink or a hard link, gets no rule.
- **Never Habakkuk's own folders.** Neither a file rule nor an exec rule covers anything in agent homes, skills, plugins, hooks, channel credentials, backups or the folders scheduled scripts run from. Hidden files and folders still ask.

`hk approvals list` shows every rule, where it applies and who saved it, and `hk approvals rm ID` removes a saved one.

These limits are known:

- **Files a tainted turn wrote.** A tainted turn can still write files in the workspace without asking. If your next turn then runs a command under a rule, and that command runs one of those files (a script, a `Makefile`, a `package.json`), the rule has run what the page wrote. The command line is the one you approved; what it runs may not be. Keep rules on commands that run workspace files off agents that read the web, or allow those commands once.
- **Names made later.** The link check looks at the files a command names as they are when it starts. A command that creates a link and then writes through it in the same line isn't caught, and neither is a program that picks its own files. That's why exec rules are exact lines you approved.
- **Commands in `full` mode.** `full` mode runs commands without asking and doesn't parse them, so a command can write an instruction file there. The file tools always ask.
- **Hard links on Windows.** Windows doesn't report a file's other names, so the hard-link check runs on macOS and Linux only.
- **Delivered messages after a restart.** Whether a delivered message came from a tainted run is known while `hk serve` runs. A message from a run that ended just before a restart is treated as clean.

## Skills and plugins from outside

Habakkuk pins and checks skills and plugins that come from outside. When you install one, it prints the source, the commit, the tree hash, every file and which of the files are scripts. Then it scans them. A high-risk finding stops the install unless you pass `-force`. The high-risk findings are `curl | sh`, decoded payloads that are run, credentials or the environment being sent out, obfuscation, and program binaries. The lock file records the commit and the hash. An update shows you a diff and applies only when you say yes.

A skill that ships scripts stays disabled until you run `hk skills approve`. Plugins stay off until you run `hk plugins enable`. `hk skills audit` scans everything again and catches files that changed after install. See [Plugins](../tools/plugins.md).

## Private files

Some of what Habakkuk writes under `$HABAKKUK_HOME` holds chats, memory, credentials, tokens or keys. That's the databases, `.env`, `auth.json`, logs, agent folders, attachments, browser profiles and backups. Habakkuk creates all of it with mode `0600`, in folders with mode `0700`. `hk doctor` and `hk security` warn you about anything wider, and `hk security -fix` tightens it. On Windows, the audit skips these Unix checks. Your profile folder there is private to you by default.

## No surprise network calls

Habakkuk doesn't send usage telemetry, and it checks for updates only when you run `hk update`. Here's what it contacts:

- your model providers and the channels you configured;
- services you named in `config.yaml`: a `search:` provider, `tts`, `images`, `video`, `realtime`, `memory.embeddings`, `telemetry.otlp_endpoint`, MCP servers, A2A peers, Home Assistant;
- the pages an agent reads with `fetch_url` or the browser.

There's no default web search provider. The `web_search` tool exists only once `search:` names one. Run `hk security -network` to list every endpoint and see whether your config turns it on.

## The local HTTP server

The HTTP server listens on loopback unless a token is set, and then refuses requests addressed to other host names (DNS rebinding). It refuses every request a browser sends from another origin, including WebSocket handshakes and form posts. Another origin means another site, another port on localhost, or a sandboxed frame. It never grants CORS. The dashboard's changes need JSON bodies, and a cross-site page can't send those without a preflight, which is refused.

This means a web page you visit can't drive the agent, read your chats or pair a device, with or without a token. Programs (curl, SDKs, webhook senders, devices) authenticate with the token, a webhook secret or a device secret.

## The audit

`hk security` runs the audit, and so does `hk security audit`. It checks:

- file modes under `$HABAKKUK_HOME`;
- agents in `full` mode or reachable by outsiders, open channels, and trusted folders (`trusted_write_paths`);
- the HTTP listener and its token, and webhook secrets;
- risky MCP servers, and skills and hooks others can write;
- backups and free disk space.

It exits non-zero for as long as a high or critical finding remains, so you can run it in a check.

## Secret managers

A setting whose name ends in `_env` holds the name of a variable. With `secrets:`, Habakkuk can resolve those variables from a secret manager instead of `.env`:

```yaml
secrets:                          # resolve *_env variables from secret managers
  sources:
    1password: {}                 # op read
    bitwarden: {}                 # bw get (ITEM, or ITEM#field)
    file: {}
    pass: {type: command, command: 'pass show "$HABAKKUK_SECRET_REF"', cache: 10m}
  refs:
    ANTHROPIC_API_KEY: op://Private/Anthropic/credential
    TELEGRAM_BOT_TOKEN: pass:habakkuk/telegram
```

Habakkuk reads a variable from the process environment first, then from a reference, then from `.env`. It redacts secret values from the log and from the dashboard's config page.

## Reporting a vulnerability

To report one, see [SECURITY.md](../../SECURITY.md) in the repository.
