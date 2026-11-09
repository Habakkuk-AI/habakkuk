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
| | | `HABAKKUK_DOWNLOAD_URL` | A mirror holding the release files |

Under `irm ... | iex` the Windows script reads its options only from the environment variables.

## Release archives

You can also download an archive from [Releases](https://github.com/habakkuk-ai/habakkuk/releases) yourself and check it against `SHA256SUMS`: `habakkuk_<os>_<arch>.tar.gz` for Linux and macOS, `habakkuk_windows_<arch>.zip` for Windows. Each holds the binary, `LICENSE`, `NOTICE` and `README.md`.

Releases are built by `.github/workflows/release.yml` whenever a `v*` tag is pushed: static binaries (no CGO) for Linux, macOS and Windows on amd64 and arm64, a `SHA256SUMS` file, a GitHub release, desktop app builds, and a multi-arch image on ghcr.io.

## From source

Build from source with Go (the version in `go.mod`):

```bash
go install github.com/habakkuk-ai/habakkuk/cmd/habakkuk@latest
```

Or from a clone, with `make build` (see [CONTRIBUTING.md](../../CONTRIBUTING.md)).

## Container image

The image runs `habakkuk serve`, with `HABAKKUK_HOME` set to `/data`:

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
