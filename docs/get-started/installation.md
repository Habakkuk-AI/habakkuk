---
title: Installation
description: "Install Habakkuk with the install script for your OS, a release archive, go install or the container image."
order: 2
---

Habakkuk is one static binary for Linux, macOS and Windows, on amd64 and arm64.

## Install script

```bash tab="macOS / Linux"
curl -fsSL https://habakkuk.ai/install.sh | sh
curl -fsSL https://habakkuk.ai/install.sh | sh -s -- --system          # /usr/local/bin, with sudo
curl -fsSL https://habakkuk.ai/install.sh | sh -s -- --version v0.3.0  # a given release
```

```powershell tab="Windows"
irm https://habakkuk.ai/install.ps1 | iex

# with options: save the script, then run it
irm https://habakkuk.ai/install.ps1 -OutFile install.ps1
.\install.ps1 -Version v0.3.0 -Dir C:\Tools\Habakkuk
```

The script downloads the release for your platform, checks it against the release's `SHA256SUMS`, and refuses to install when the checksum does not match.

- **macOS and Linux** put `habakkuk` in `~/.local/bin` and tell you when that folder is not on your `PATH`. Under Rosetta on Apple silicon it still picks the arm64 build.
- **Windows** puts `habakkuk.exe` in `%LOCALAPPDATA%\Programs\Habakkuk` and adds it to your user `PATH`. No admin rights are needed.
- **`hk`, the short name.** Both scripts add `hk` beside `habakkuk` (a link on macOS and Linux, `hk.cmd` on Windows), so every command works either way: `hk chat` is `habakkuk chat`. If a different program named `hk` is already in that folder, it is left alone and the script says so; it also warns when another `hk` comes first on your `PATH`.

Both scripts are in the repository ([`scripts/install.sh`](../../scripts/install.sh), [`scripts/install.ps1`](../../scripts/install.ps1)), so you can also fetch them from GitHub:

```bash
curl -fsSL https://raw.githubusercontent.com/habakkuk-ai/habakkuk/main/scripts/install.sh | sh
```

### Options

| `install.sh` | `install.ps1` | Environment variable | Meaning |
|---|---|---|---|
| `--version TAG` | `-Version TAG` | `HABAKKUK_VERSION` | Install this release, e.g. `v0.3.0` (default: the latest) |
| `--dir DIR` | `-Dir DIR` | `HABAKKUK_INSTALL_DIR` | Install into `DIR` |
| `--system` | | `HABAKKUK_SYSTEM=1` | Install into `/usr/local/bin`, with sudo if needed |
| | `-NoPath` | | Do not add the folder to your user `PATH` |
| `--no-hk` | `-NoHk` | `HABAKKUK_NO_HK=1` | Do not add `hk`, the short name |
| | | `HABAKKUK_DOWNLOAD_URL` | A mirror holding the release files |

Under `irm ... | iex` the Windows script reads its options only from the environment variables.

### Uninstall

Remove the binary and the short name. Your agents, chats and keys are in the data folder (`~/.habakkuk`, or `%APPDATA%\Habakkuk` on Windows) and stay until you delete that too, so installing again picks up where you left off.

```bash tab="macOS / Linux"
rm ~/.local/bin/habakkuk ~/.local/bin/hk     # /usr/local/bin after --system
rm -rf ~/.habakkuk                            # only if you want the data gone as well
```

```powershell tab="Windows"
Remove-Item -Recurse "$env:LOCALAPPDATA\Programs\Habakkuk"
Remove-Item -Recurse "$env:APPDATA\Habakkuk"   # only if you want the data gone as well
```

If you let `habakkuk setup` install the background service, stop it first: `systemctl --user disable --now habakkuk` on Linux, or `launchctl bootout gui/$(id -u)/ai.habakkuk.agent` on macOS (then delete `~/Library/LaunchAgents/ai.habakkuk.agent.plist`). On Windows the installer also added the folder to your user `PATH`; remove it under Settings > System > About > Advanced system settings > Environment Variables.

## Release archives

You can also download an archive from [Releases](https://github.com/habakkuk-ai/habakkuk/releases) yourself and check it against `SHA256SUMS`: `habakkuk_<os>_<arch>.tar.gz` for Linux and macOS, `habakkuk_windows_<arch>.zip` for Windows. Each holds the binary, `LICENSE`, `NOTICE` and `README.md`.

Releases are built by `.github/workflows/release.yml` whenever a `v*` tag is pushed: static binaries (no CGO) for Linux, macOS and Windows on amd64 and arm64, a `SHA256SUMS` file and its signature `SHA256SUMS.sig`, a GitHub release, desktop app builds, and a multi-arch image on ghcr.io. The install scripts check the archive against `SHA256SUMS`; `habakkuk update` also checks the signature with the release key built into the binary.

## From source

Build from source with Go (the version in `go.mod`):

```bash
go install github.com/habakkuk-ai/habakkuk/cmd/habakkuk@latest
```

Or from a clone, with `make build` (see [CONTRIBUTING.md](../../CONTRIBUTING.md)).

`go install` gives you `habakkuk` only. For the short name, add the link yourself:

```bash
ln -s habakkuk "$(go env GOPATH)/bin/hk"
```

## Container image

The image runs `habakkuk serve`, with `HABAKKUK_HOME` set to `/data`. `hk` works inside it too (`docker exec habakkuk hk status`):

```bash
docker run --rm -it -v habakkuk-data:/data ghcr.io/habakkuk-ai/habakkuk:latest init   # once
docker run -d --name habakkuk -v habakkuk-data:/data ghcr.io/habakkuk-ai/habakkuk:latest
```

The image is Debian slim with `ca-certificates`, `bash`, `git` and `curl`. For the browser toolset or meetings, build the variant with Chromium (about 1 GB more, unpacked):

```bash
docker build --target browser -t habakkuk:browser .
```

[Run it on a server](../deploy/server.md#in-a-container) covers ports, keys and restarts.

## Windows

Everything runs natively except computer use (`computer_*` tools), which reports that it is not supported on Windows yet. Run Habakkuk in WSL for that.

- **Data** lives in `%APPDATA%\Habakkuk` (`$env:HABAKKUK_HOME` overrides it), so `habakkuk init` writes `%APPDATA%\Habakkuk\config.yaml` and keys go in `%APPDATA%\Habakkuk\.env`.
- **Commands** the agent runs with `exec`, and hooks, scheduled scripts and `command` secret sources, run in PowerShell (`pwsh` when installed, else Windows PowerShell 5.1). Set `HABAKKUK_SHELL` to `cmd`, `powershell`, `pwsh`, or `bash`/`sh` (for example Git Bash on `PATH`) to use another shell. Docker and SSH sandboxes still run `sh` on their side; a Docker sandbox mounts the workspace at `/workspace`.
- **The browser** tools find Chrome, Chromium or Edge in their usual install folders and drive them over a loopback-only DevTools port.
- **Plugin scripts** read their arguments from `$env:HABAKKUK_ARGS`. Plugin hooks run in the plugin's folder, so ship `.ps1` scripts (or `.cmd`) for Windows users.
- **The security audit** cannot judge Windows ACLs and skips the Unix permission checks. Your profile folder is private to you by default.

> [!NOTE]
> Windows builds are new. The [roadmap](../contributing/roadmap.md#known-gaps-on-windows) lists what has not been tested on a real Windows machine yet.

## Desktop app

Habakkuk Desktop, a native window around the dashboard for macOS, Windows and Linux, is attached to each release. See [Desktop app](../interfaces/desktop-app.md).

## Next steps

<!-- cards -->
- [Quickstart](quickstart.md): run the setup wizard and get a first reply.
- [Updating](updating.md): move to a new release, and roll back if you need to.
