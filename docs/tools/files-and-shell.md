---
title: Files and shell
description: "The core workspace tools: file reads and edits, exec, apply_patch, search, background processes, Docker and SSH sandboxes, and workspace checkpoints with rollback."
order: 1
---

Every agent has a workspace: the folder its file tools and commands work in. Set it with `workspace:`; without it, the agent uses `$HABAKKUK_HOME/agents/<name>/workspace`.

```yaml
agents:
  habakkuk:
    workspace: ~/habakkuk-workspace
```

## Workspace tools

The `core` toolset has:

| Tool | What it does |
|---|---|
| `read_file`, `list_dir` | Read a file or list a folder |
| `write_file`, `edit_file` | Write a file, or replace text in one |
| `apply_patch` | Change many files at once, from a unified diff or the `*** Begin Patch` format. Every hunk is applied in memory before anything is written |
| `search_files` | A regular expression search, with `rg` when it is installed |
| `exec` | Run a command |
| `fetch_url` | Fetch a page or file over HTTP(S) |
| `todo` | A checklist the agent keeps for the task in progress |
| `clarify` | Ask you a question and wait for the answer |

In `workspace` mode, reads and writes inside the workspace run on their own; `exec` and writes outside need approval. See [Permissions and approvals](../agents/approvals.md).

Files people send in chats are copied to `workspace/inbox/`, so tools can open them.

## Background processes

`exec` with `background: true` starts the command and returns a process id at once. Then:

- `process_list`, `process_output`, `process_wait`, `process_kill` and `process_write` act on it;
- output goes to `$HABAKKUK_HOME/procs/<id>.log`, capped;
- with `notify: true`, a process that exits starts a turn that tells the agent.

Processes live as long as the Habakkuk process.

## Sandboxes

An agent's `sandbox` decides where `exec` and background processes run:

| `sandbox` | Commands run | File tools |
|---|---|---|
| `none` (default) | Here, each in its own process group (with bash; PowerShell on Windows) | The local workspace |
| `docker` | In one long-lived container per agent (`habakkuk-<agent>`) | The local workspace, bind-mounted at the same path |
| `ssh` | On a remote host, in `workdir` | Reads stay local; `write_file`, `edit_file` and `apply_patch` are refused; change files there with `exec` |

```yaml
agents:
  coder:
    sandbox: docker
    docker: {image: "debian:stable-slim", network: false, cpus: "2", memory: 2g}   # also: user
  remote:
    sandbox: ssh
    ssh: {host: build.example.com, user: habakkuk, key_env: BUILD_SSH_KEY, workdir: /srv/work}   # also: port
```

Both drive the `docker` and `ssh` command-line tools, which must be on `PATH`. On Windows, a Docker sandbox mounts the workspace at `/workspace`. Agents whose commands run in a sandbox cannot use [computer use](computer-use.md), which is on this machine. See [Architecture: sandboxes](../reference/architecture.md#sandboxes).

## Checkpoints

Before the first call in a turn that may change the workspace, Habakkuk snapshots it into a shadow git repository in `$HABAKKUK_HOME/checkpoints/<agent>`. The workspace's own `.git` is never touched. It needs `git` on `PATH`.

```bash
habakkuk checkpoints [-agent A]             # workspace snapshots taken before turns changed files
habakkuk rollback [-agent A] [-y] ID [PATH] # restore the workspace, or one path, to a snapshot
```

`/undo` in a chat restores the workspace to before the conversation's latest turn that changed it, and walks further back when repeated. Send `/stop` first if a turn is running.

```yaml
agents:
  habakkuk:
    checkpoints: true        # the default; false turns snapshots off
    checkpoint_max_mb: 200   # skip a snapshot whose new and changed files are larger
```

## Safety guards

Some commands are never run, in any permission mode: recursive deletion of `/` or your home, `mkfs`, `dd` onto a disk, fork bombs, and a remote download piped into a shell. `fetch_url` refuses loopback, LAN and cloud metadata addresses unless `tools.allow_private_urls` is set. See [Safety guards](../agents/approvals.md#safety-guards).
