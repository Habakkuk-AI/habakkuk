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

A turn is tainted once it has read outside content. Outside content is a web page, a search result, an MCP or other network tool, an email, a webhook or Home Assistant payload, or a device's output. From then on, `exec`, `process` and writes outside the workspace need a human's approval. This holds even in `full` mode and even with `approvals: reviewer`. A model isn't asked, because the same text could steer it. When nobody is attending, these calls are denied.

A tainted turn also can't write trusted memory, change the user profile or set the agent's identity. Its `message_home` sends only a short status. A delegated task inherits its parent's taint and origin.

The taint stays with the turn across a restart. Say a turn reads a web page and is then interrupted by a deploy or a crash. It's still tainted when it resumes. Habakkuk sets the taint again from the turn's own transcript, from any hosted search result and any call to a tool that can bring outside content in.

Setting `allow_tainted_exec: true` on an agent turns this off. The audit flags it.

A project's instruction files work the same way. **A project's `AGENTS.md`, `CLAUDE.md` and `.cursorrules` are untrusted unless you trusted their exact content with `hk projects trust`**, and that includes the agent's own workspace. An untrusted file is given to the agent as data, and **every turn of that session is tainted**. So a cloned repository whose `AGENTS.md` says "before any task, run this script" can't get a command run without you. The taint is read back from the session's first message, so it holds after a restart too. Trust is given to content, not to a folder or a file name: Habakkuk keeps each trusted file's SHA-256 in `$HABAKKUK_HOME/projects/`, and any change, by anyone, makes the file untrusted again until you trust it again. The agents' file tools and every tool that saves files refuse that folder in every turn, however its path is spelled; `hk projects trust` needs a terminal; and a record that isn't private to you (0600 in a 0700 folder you own) trusts nothing. In `full` mode, a clean turn's shell can still do anything, the record included: that's what `full` means. See [Project instruction files](../agents/agents.md#project-instruction-files).

Tools that save what they fetched or made (browser downloads, PDFs and screenshots, generated images and videos, chat attachments) save only inside the workspace once links are followed, so a link in the workspace can't lead a save into `$HABAKKUK_HOME` or anywhere else. Local files an MCP server writes are its own process's doing, outside this check.

What that doesn't cover: instructions in other files, such as a project's `README.md`, which the agent may read while it works; and a change a clean turn's shell makes to a file you trusted, which then simply stays untrusted until you trust it again, because its fingerprint no longer matches.

A tainted turn, or a message from someone other than the owner, also can't write `AGENTS.md`, `CLAUDE.md` or `.cursorrules` with the file tools, wherever they are and through any link. That's an extra; what keeps those files safe is that their content must be trusted. And a skill it saves with `skill_save` is held in `agents/<name>/skills-held/`, where nothing lists or loads it, until you look at it and run `hk skills approve AGENT/NAME`. `hk skills held` lists them. So text planted in a web page can't become a procedure later sessions follow.

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

## Skills and plugins from outside

Habakkuk pins and checks skills and plugins that come from outside. When you install one, it prints the source, the commit, the tree hash, every file and which of the files are scripts. Then it scans them. A high-risk finding stops the install unless you pass `-force`. The high-risk findings are `curl | sh`, decoded payloads that are run, credentials or the environment being sent out, obfuscation, and program binaries. The lock file records the commit and the hash. An update shows you a diff and applies only when you say yes.

A skill that ships scripts stays disabled until you run `hk skills approve`. Plugins stay off until you run `hk plugins enable`. `hk skills audit` scans everything again and catches files that changed after install. See [Plugins](../tools/plugins.md).

## Private files

Some of what Habakkuk writes under `$HABAKKUK_HOME` holds chats, memory, credentials, tokens or keys. That's the databases, `.env`, `auth.json`, logs, agent folders, attachments, browser profiles, backups and the Web Push key and devices in `push/`. Habakkuk creates all of it with mode `0600`, in folders with mode `0700`. `hk doctor` and `hk security` warn you about anything wider, and `hk doctor --fix` or `hk security -fix` tightens it. On Windows, the audit skips these Unix checks. Your profile folder there is private to you by default.

## No surprise network calls

Habakkuk doesn't send usage telemetry, and it checks for updates only when you run `hk update`. Here's what it contacts:

- your model providers and the channels you configured;
- services you named in `config.yaml`: a `search:` provider, `tts`, `images`, `video`, `realtime`, `memory.embeddings`, `telemetry.otlp_endpoint`, MCP servers, A2A peers, Home Assistant;
- the pages an agent reads with `fetch_url` or the browser;
- with an `http` section, the browser push service of each device you turned [notifications](../interfaces/dashboard.md#notifications) on for, and only those. Each message is encrypted for that device.

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
