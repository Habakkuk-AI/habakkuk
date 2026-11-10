---
title: Files and shell
description: "The core tools for files, commands, patches and search, plus background processes, Docker and SSH sandboxes, and checkpoints you can roll back to."
order: 1
---

An agent works with files and runs commands through the tools described here. You can sandbox those commands, and undo what a turn changed.

Every agent has a workspace, which is the folder its file tools and commands work in. Set it with `workspace:`. If you don't, the agent uses `$HABAKKUK_HOME/agents/<name>/workspace`.

```yaml
agents:
  habakkuk:
    workspace: ~/habakkuk-workspace
```

## Workspace tools

These tools are in the `core` toolset:

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

In `workspace` mode, reads and writes inside the workspace run without asking. `exec` needs approval, and so do writes outside the workspace. See [Permissions and approvals](../agents/approvals.md).

When someone sends a file in a chat, it's copied to `workspace/inbox/` so the tools can open it.

## Background processes

Call `exec` with `background: true` to start a command and get a process id back straight away. After that:

- `process_list`, `process_output`, `process_wait`, `process_kill` and `process_write` work on that process.
- Its output goes to `$HABAKKUK_HOME/procs/<id>.log`. The log is capped.
- With `notify: true`, the process starts a turn when it exits, and that turn tells the agent.

Background processes live as long as the Habakkuk process does.

## Sandboxes

The `sandbox` setting on an agent decides where `exec` and background processes run:

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

The two sandboxes work by driving the `docker` and `ssh` command-line tools, so those have to be on `PATH`. On Windows, a Docker sandbox mounts the workspace at `/workspace`. An agent whose commands run in a sandbox can't use [computer use](computer-use.md), because that happens on this machine. For more, see [Architecture: sandboxes](../reference/architecture.md#sandboxes).

## Checkpoints

A checkpoint is a snapshot of the workspace that you can roll back to. Habakkuk takes one before the first call in a turn that may change the workspace. It keeps the snapshot in a shadow git repository in `$HABAKKUK_HOME/checkpoints/<agent>` and never touches the workspace's own `.git`. Checkpoints need `git` on `PATH`.

From the terminal, you can list the snapshots and roll back to one:

```bash
hk checkpoints [-agent A]                   # workspace snapshots taken before turns changed files
hk rollback [-agent A] [-y] ID [PATH] # restore the workspace, or one path, to a snapshot
```

In a chat, send `/undo` to put the workspace back to how it was before the conversation's latest turn that changed it. Send it again to walk further back. If a turn is running, send `/stop` first.

Checkpoints are on by default. Two settings on the agent control them:

```yaml
agents:
  habakkuk:
    checkpoints: true        # the default; false turns snapshots off
    checkpoint_max_mb: 200   # skip a snapshot whose new and changed files are larger
```

## Safety guards

Habakkuk refuses to run some commands in every permission mode. These are recursive deletion of `/` or your home, `mkfs`, `dd` onto a disk, fork bombs, and a remote download piped into a shell. `fetch_url` won't fetch loopback, LAN or cloud metadata addresses unless you set `tools.allow_private_urls`. For more, see [Safety guards](../agents/approvals.md#safety-guards).
