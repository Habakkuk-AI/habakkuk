---
title: Remote CLI
description: "Save your server as a remote, then use hk -r NAME on your laptop to run any command there over ssh, or status and chat over HTTPS."
order: 4
---

Your main agent may run on a server, under a service manager (see [Run it on a server](../deploy/server.md)). You can still work with it from your laptop, using `hk`. Save the server once as a remote (a saved name for that server). After that, put `-r NAME` before any command:

```bash
hk chat                 # the Habakkuk on this laptop, as always
hk -r srv chat          # the Habakkuk on the server
hk -r srv status
hk -r srv logs -f
```

You don't change anything on the server. A remote command is the same command you'd type there, and it runs there.

## Add an ssh remote

An ssh remote runs every command. To use one, you need the OpenSSH client, which is built into macOS, Linux and Windows 10 and later. You also need a login to the server that works without a password prompt. A key or ssh-agent gives you that.

```bash
ssh you@server.example.com echo ok     # check you get in without a password prompt
hk remote add srv ssh://you@server.example.com
hk -r srv status
```

When you run `hk -r srv COMMAND`, hk runs `ssh you@server.example.com habakkuk COMMAND`. It quotes every argument, so spaces, quotes and `$` reach the server unchanged. If you're in a terminal, ssh gets a terminal too. That's why `chat`, the terminal UI and the setup wizards work. With a pipe, ssh gets no terminal, so these work as well:

```bash
hk -r srv status | grep queue
hk -r srv sessions export ses_123 -format md > chat.md
```

The exit code you get is the one from the command on the server. If ssh can't connect, ssh itself exits with 255.

| Flag | |
|---|---|
| `ssh://user@host:port` | The target. A host alias from `~/.ssh/config` works too: `ssh://srv`. |
| `-identity PATH` | A private key file (`ssh -i`). Without it, ssh uses its config and agent. |
| `-home DIR` | The `HABAKKUK_HOME` on the server. Without it, the server's environment decides (`~/.habakkuk` by default). Set it when the service uses another home. `~/` means the server user's home. |
| `-bin PATH` | The `habakkuk` binary on the server. Without it, `habakkuk` from the `PATH` of the ssh session, then `~/.local/bin/habakkuk`, where the installer puts it. |
| `-default` | Make it the default remote (below). |
| `-force` | Replace a remote with the same name. |

> [!NOTE]
> The login shell on the server must be a POSIX shell (sh, bash, dash or zsh). That's the default on Linux servers.

## Commands that change the server

Some commands change what's installed on the server, or start a second server next to the service. These won't run on a remote unless you put `--yes-remote` before the command:

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

You don't need the flag for `update -check`, `migrate -dry-run` or `config migrate -dry-run`.

## Add an https remote

An https remote needs no ssh login. It works when you can reach the server's [HTTP API](../reference/api.md), either behind HTTPS or through `hk expose tailscale`. It runs two commands:

- `status` shows health, queue, approvals, board, schedules, channels and recent errors.
- `chat` opens a line-by-line chat in a web conversation. By default that conversation is `cli-<this computer's name>`. Use `-agent A` to pick the agent, and `-conversation ID` to pick another conversation. Type `/exit` to quit. Other `/commands` go to the agent, as in any chat.

If you try any other command, it tells you it needs an ssh remote.

```bash
export HK_SERVER_TOKEN=...            # the server's http.token_env value
hk remote add web https://habakkuk.example.com -token-env HK_SERVER_TOKEN
hk -r web chat
echo "what is on my board?" | hk -r web chat
```

`-token-env` takes the name of the variable that holds the token on your laptop. hk never saves the token itself. Plain `http://` is accepted only for a loopback address, such as an ssh tunnel. For example, run `ssh -L 8484:127.0.0.1:8484 srv`, then `hk remote add tunnel http://127.0.0.1:8484`.

## Pick the remote

Put `-r NAME` (or `--remote NAME`) before the command to say where it runs. If you leave it out, hk decides in this order:

1. If `HABAKKUK_REMOTE=NAME` is set, it picks the remote for the commands in that shell.
2. If you've run `hk remote default NAME`, plain `hk` commands run on that remote.
3. Otherwise commands run on this machine.

To be sure a command runs here, use `-r local` (or `HABAKKUK_REMOTE=local`). `hk remote ...` itself always runs here too.

When a command runs on a remote and stderr is a terminal, hk prints a dim `(remote: NAME)` line first. To hide it, put `-q` before the command.

## Manage remotes

```bash
hk remote list                      # * marks the default
hk remote default srv               # or: hk remote default none
hk remote rm srv
```

hk keeps your remotes in `remotes.json`, which only you can read. The file is in your user config directory: `~/.config/habakkuk` on Linux, `~/Library/Application Support/habakkuk` on macOS, and `%AppData%\habakkuk` on Windows. It holds targets, paths and variable names. It never holds a token or password.
