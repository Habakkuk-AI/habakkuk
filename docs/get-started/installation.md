---
title: Installation
description: "Install Habakkuk with the install script for your OS, a release archive, go install or the container image."
order: 2
---

There are four ways to install Habakkuk: the install script, a release archive, a build from source or the container image. Uninstalling and what's different on Windows are further down.

Habakkuk is one static binary (a single program file with no libraries to install beside it). There are builds for Linux, macOS and Windows, on amd64 and arm64.

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

The script downloads the release for your platform and checks it against the release's `SHA256SUMS`. If the checksum doesn't match, it refuses to install.

- **On macOS and Linux**, the script puts `habakkuk` in `~/.local/bin` and tells you if that folder isn't on your `PATH`. Under Rosetta on Apple silicon, it still picks the arm64 build.
- **On Windows**, the script puts `habakkuk.exe` in `%LOCALAPPDATA%\Programs\Habakkuk` and adds that folder to your user `PATH`. You don't need admin rights.
- **`hk`, the short name.** Both scripts add `hk` beside `habakkuk` (a link on macOS and Linux, `hk.cmd` on Windows). Every command works under either name, so `hk chat` is the same as `habakkuk chat`. These docs use `hk`. If a different program named `hk` is already in that folder, the script leaves it alone and tells you. It also warns you when another `hk` comes first on your `PATH`.

Both scripts live in the repository, as [`scripts/install.sh`](../../scripts/install.sh) and [`scripts/install.ps1`](../../scripts/install.ps1), so you can fetch them from GitHub too:

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

When you run the Windows script with `irm ... | iex`, it reads its options only from the environment variables.

### Uninstall

To uninstall, remove the binary and the short name. Your agents, chats and keys live in the data folder (`~/.habakkuk`, or `%APPDATA%\Habakkuk` on Windows). They stay there until you delete that folder too, so if you install again you pick up where you left off.

```bash tab="macOS / Linux"
rm ~/.local/bin/habakkuk ~/.local/bin/hk     # /usr/local/bin after --system
rm -rf ~/.habakkuk                            # only if you want the data gone as well
```

```powershell tab="Windows"
Remove-Item -Recurse "$env:LOCALAPPDATA\Programs\Habakkuk"
Remove-Item -Recurse "$env:APPDATA\Habakkuk"   # only if you want the data gone as well
```

Did you let `hk setup` install the background service? Stop it first. On Linux, run `systemctl --user disable --now habakkuk`. On macOS, run `launchctl bootout gui/$(id -u)/ai.habakkuk.agent`, then delete `~/Library/LaunchAgents/ai.habakkuk.agent.plist`. On Windows, the installer also added the folder to your user `PATH`. Remove it under Settings > System > About > Advanced system settings > Environment Variables.

## Release archives

You can also download an archive yourself from [Releases](https://github.com/habakkuk-ai/habakkuk/releases) and check it against `SHA256SUMS`. The archive is `habakkuk_<os>_<arch>.tar.gz` for Linux and macOS, and `habakkuk_windows_<arch>.zip` for Windows. Each one holds the binary, `LICENSE`, `NOTICE` and `README.md`.

`.github/workflows/release.yml` builds a release whenever a `v*` tag is pushed. A release is made of static binaries (no CGO) for Linux, macOS and Windows on amd64 and arm64, a `SHA256SUMS` file and its signature `SHA256SUMS.sig`, a GitHub release, desktop app builds, and a multi-arch image on ghcr.io. The install scripts check the archive against `SHA256SUMS`. `hk update` checks the signature as well, using the release key built into the binary.

## From source

To build from source, you need Go, at the version named in `go.mod`. Then run:

```bash
go install github.com/habakkuk-ai/habakkuk/cmd/habakkuk@latest
```

If you're working from a clone, run `make build` there (see [CONTRIBUTING.md](../../CONTRIBUTING.md)).

`go install` gives you `habakkuk` only. These docs use the short name, `hk`, so add the link yourself:

```bash
ln -s habakkuk "$(go env GOPATH)/bin/hk"
```

## Container image

The container image runs `hk serve` and sets `HABAKKUK_HOME` to `/data`. The short name `hk` works inside it too, as in `docker exec habakkuk hk status`. Here are the commands:

```bash
docker run --rm -it -v habakkuk-data:/data ghcr.io/habakkuk-ai/habakkuk:latest init   # once
docker run -d --name habakkuk -v habakkuk-data:/data ghcr.io/habakkuk-ai/habakkuk:latest
```

The image is Debian slim with `ca-certificates`, `bash`, `git` and `curl`. If you want the browser toolset or meetings, build the variant with Chromium. It's about 1 GB larger, unpacked:

```bash
docker build --target browser -t habakkuk:browser .
```

For ports, keys and restarts, see [Run it on a server](../deploy/server.md#in-a-container).

## Windows

On Windows, everything runs natively except computer use (the `computer_*` tools). Computer use reports that it isn't supported on Windows yet, so run Habakkuk in WSL if you need it.

- **Data** lives in `%APPDATA%\Habakkuk`, unless you set `$env:HABAKKUK_HOME` to another folder. That means `hk init` writes `%APPDATA%\Habakkuk\config.yaml`, and your keys go in `%APPDATA%\Habakkuk\.env`.
- **Commands** run in PowerShell. That covers the commands the agent runs with `exec`, plus hooks, scheduled scripts and `command` secret sources. Habakkuk uses `pwsh` when it's installed, and Windows PowerShell 5.1 otherwise. To use another shell, set `HABAKKUK_SHELL` to `cmd`, `powershell`, `pwsh`, or `bash`/`sh` (for example Git Bash on `PATH`). Docker and SSH sandboxes still run `sh` on their side, and a Docker sandbox mounts the workspace at `/workspace`.
- **The browser** tools look for Chrome, Chromium or Edge in their usual install folders. They drive the browser over a loopback-only DevTools port, which only this computer can reach.
- **Plugin scripts** get their arguments from `$env:HABAKKUK_ARGS`. Plugin hooks run in the plugin's folder, so if you write a plugin, ship `.ps1` scripts (or `.cmd`) for Windows users.
- **The security audit** can't judge Windows ACLs, and it skips the Unix permission checks. Your profile folder is private to you by default.

> [!NOTE]
> The Windows builds are new. See the [roadmap](../contributing/roadmap.md#known-gaps-on-windows) for what hasn't been tested on a real Windows machine yet.

## Desktop app

Habakkuk Desktop is a native window around the dashboard, for macOS, Windows and Linux. It's attached to each release. See [Desktop app](../interfaces/desktop-app.md).

## Next steps

<!-- cards -->
- [Quickstart](quickstart.md): run the setup wizard and get your first reply.
- [Updating](updating.md): move to a new release, or roll back if you need to.
