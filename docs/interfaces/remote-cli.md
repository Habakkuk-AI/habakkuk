---
title: Remote CLI
description: "Run hk on your laptop against the Habakkuk on your server: save the server as a remote, then hk -r NAME runs any command there over ssh, or status and chat over HTTPS."
order: 4
---

Your main agent may run on a server, under a service manager (see [Run it on a server](../deploy/server.md)). You can still use `hk` on your laptop for it. Save the server once as a remote, then put `-r NAME` before any command:

```bash
hk chat                 # the Habakkuk on this laptop, as always
hk -r srv chat          # the Habakkuk on the server
hk -r srv status
hk -r srv logs -f
```

Nothing changes on the server. A remote command is the same command, run there.

## Add an ssh remote

An ssh remote runs every command. You need the OpenSSH client (built into macOS, Linux and Windows 10 and later) and a login to the server that works without a password prompt: a key, or ssh-agent.

```bash
ssh you@server.example.com echo ok     # check you get in without a password prompt
hk remote add srv ssh://you@server.example.com
hk -r srv status
```

`hk -r srv COMMAND` runs `ssh you@server.example.com habakkuk COMMAND`, with every argument quoted, so spaces, quotes and `$` reach the server unchanged. ssh gets a terminal when yours is one, so `chat`, the terminal UI and the setup wizards work. With a pipe it gets none, so this works:

```bash
hk -r srv status | grep queue
hk -r srv sessions export ses_123 -format md > chat.md
```

The exit code is the server command's (ssh itself exits 255 when it cannot connect).

| Flag | |
|---|---|
| `ssh://user@host:port` | The target. A host alias from `~/.ssh/config` works too: `ssh://srv`. |
| `-identity PATH` | A private key file (`ssh -i`). Without it, ssh uses its config and agent. |
| `-home DIR` | The `HABAKKUK_HOME` on the server. Without it, the server's environment decides (`~/.habakkuk` by default). Set it when the service uses another home. `~/` means the server user's home. |
| `-bin PATH` | The `habakkuk` binary on the server. Without it, `habakkuk` from the `PATH` of the ssh session, then `~/.local/bin/habakkuk`, where the installer puts it. |
| `-default` | Make it the default remote (below). |
| `-force` | Replace a remote with the same name. |

> [!NOTE]
> The server's login shell must be a POSIX shell (sh, bash, dash or zsh), the default on Linux servers.

## Commands that change the server

Some commands change what is installed on the server, or start a second server beside the service. They refuse to run on a remote unless you add `--yes-remote` before the command:

| Command | Why it asks |
|---|---|
| `serve` | The service already runs it. A second one in your ssh session stops when you disconnect. |
| `init`, `restore` | They write the server's home. |
| `update` | It replaces the server's binary. Restart the service after it. |
| `migrate`, `config migrate` | They rewrite the server's data or config. |
| `setup service` | It installs a service on the server. |

```bash
hk -r srv update -check               # read-only forms run without asking
hk -r srv --yes-remote update
```

`update -check`, `migrate -dry-run` and `config migrate -dry-run` run without it.

## Add an https remote

If the server's [HTTP API](../reference/api.md) is reachable (behind HTTPS, or through `habakkuk expose tailscale`), an https remote needs no ssh login. It runs two commands:

- `status`: health, queue, approvals, board, schedules, channels and recent errors.
- `chat`: a line-by-line chat in a web conversation, `cli-<this computer's name>` by default. `-agent A` picks the agent, `-conversation ID` another conversation. Type `/exit` to quit; other `/commands` go to the agent as in any chat.

Other commands say they need an ssh remote.

```bash
export HK_SERVER_TOKEN=...            # the server's http.token_env value
hk remote add web https://habakkuk.example.com -token-env HK_SERVER_TOKEN
hk -r web chat
echo "what is on my board?" | hk -r web chat
```

`-token-env` is the name of the variable that holds the token on your laptop. The token itself is never saved. Plain `http://` is accepted only for a loopback address, such as an ssh tunnel: `ssh -L 8484:127.0.0.1:8484 srv`, then `hk remote add tunnel http://127.0.0.1:8484`.

## Pick the remote

`-r NAME` (or `--remote NAME`) comes before the command. Without it:

1. `HABAKKUK_REMOTE=NAME` picks a remote for the commands in that shell.
2. `hk remote default NAME` makes plain `hk` commands run there.
3. Otherwise commands run on this machine.

`-r local` (or `HABAKKUK_REMOTE=local`) always runs here. `hk remote ...` itself always runs here.

When a command runs on a remote and stderr is a terminal, hk first prints a dim `(remote: NAME)` line. `-q` before the command hides it.

## Manage remotes

```bash
hk remote list                      # * marks the default
hk remote default srv               # or: hk remote default none
hk remote rm srv
```

Remotes are kept in `remotes.json` in your user config directory (`~/.config/habakkuk` on Linux, `~/Library/Application Support/habakkuk` on macOS, `%AppData%\habakkuk` on Windows), readable only by you. It holds targets, paths and variable names, never a token or password.
