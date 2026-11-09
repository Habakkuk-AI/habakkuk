---
title: Installation
description: Install Habakkuk with the install script, a release archive, go install or the container image.
order: 1
---

## Install script

On Linux or macOS (amd64 or arm64), the install script downloads the latest release, checks it against the release's `SHA256SUMS` and puts `habakkuk` in `~/.local/bin`:

```bash
curl -fsSL https://habakkuk.ai/install.sh | sh
curl -fsSL https://habakkuk.ai/install.sh | sh -s -- --system          # /usr/local/bin, with sudo
curl -fsSL https://habakkuk.ai/install.sh | sh -s -- --version v0.3.0  # a given release
```

The same script is in the repository at [`scripts/install.sh`](../../scripts/install.sh), so you can also fetch it straight from GitHub:

```bash
curl -fsSL https://raw.githubusercontent.com/habakkuk-ai/habakkuk/main/scripts/install.sh | sh
```

Options:

| Option | Environment variable | Meaning |
|---|---|---|
| `--version TAG` | `HABAKKUK_VERSION` | install this release, e.g. `v0.3.0` (default: the latest) |
| `--dir DIR` | `HABAKKUK_INSTALL_DIR` | install into `DIR` (default: `~/.local/bin`) |
| `--system` | `HABAKKUK_SYSTEM=1` | install into `/usr/local/bin`, with sudo if needed |
| | `HABAKKUK_DOWNLOAD_URL` | a mirror holding the release files |

The script refuses to install when the checksum does not match, and tells you when the install folder is not on your `PATH`. Under Rosetta on Apple silicon it still picks the arm64 build.

## Windows

Everything runs natively except computer use (`computer_*` tools), which reports that it is not supported on Windows yet; run Habakkuk in WSL for that.

- **Data** lives in `%APPDATA%\Habakkuk` (`$env:HABAKKUK_HOME` overrides it), so `habakkuk init` writes `%APPDATA%\Habakkuk\config.yaml` and keys go in `%APPDATA%\Habakkuk\.env`.
- **Commands** the agent runs with `exec`, and hooks, scheduled scripts and `command` secret sources, run in PowerShell (`pwsh` when installed, else Windows PowerShell 5.1). Set `HABAKKUK_SHELL` to `cmd`, `powershell`, `pwsh`, or `bash`/`sh` (for example Git Bash on `PATH`) to use another shell. Docker and SSH sandboxes still run `sh` on their side; a Docker sandbox mounts the workspace at `/workspace`.
- **The browser** tools find Chrome, Chromium or Edge in their usual install folders and drive them over a loopback-only DevTools port.
- **Plugin scripts** read their arguments from `$env:HABAKKUK_ARGS`; plugin hooks run in the plugin's folder, so ship `.ps1` scripts (or `.cmd`) for Windows users.
- **Security audit** cannot judge Windows ACLs and skips the Unix permission checks; your profile folder is private to you by default.

## Release archives

You can also download a `habakkuk_<os>_<arch>.tar.gz` from [Releases](https://github.com/habakkuk-ai/habakkuk/releases) yourself and check it against `SHA256SUMS`. Each archive holds the `habakkuk` binary, `LICENSE` and `README.md`.

Releases are built by `.github/workflows/release.yml` whenever a `v*` tag is pushed: static binaries (no CGO) for linux and darwin on amd64 and arm64, a `SHA256SUMS` file, a GitHub release, desktop app builds, and a multi-arch image on ghcr.io. Windows binaries are not built yet.

## From source

Build from source with Go (the version in `go.mod`):

```bash
go install github.com/habakkuk-ai/habakkuk/cmd/habakkuk@latest
```

or from a clone, with `make build` (see [CONTRIBUTING.md](../../CONTRIBUTING.md)).

## Container image

Run the container image (`HABAKKUK_HOME` is `/data`):

```bash
docker run -d --name habakkuk -v habakkuk-data:/data ghcr.io/habakkuk-ai/habakkuk:latest   # runs habakkuk serve
docker run --rm -it -v habakkuk-data:/data ghcr.io/habakkuk-ai/habakkuk:latest init
```

The image is Debian slim with `ca-certificates`, `bash`, `git` and `curl`. For the browser toolset, build the variant with Chromium (about 1 GB more, unpacked):

```bash
docker build --target browser -t habakkuk:browser .
```

## Desktop app

Habakkuk Desktop, a native window around the dashboard for macOS, Windows and Linux, is attached to each release. See [Desktop app](../guides/desktop-app.md).

## Updating

```bash
habakkuk update -check            # is there a newer release on your channel?
habakkuk update                   # install it
habakkuk update -channel beta     # follow pre-releases too (remembered; -channel stable goes back)
habakkuk update --rollback        # undo the last update: binary, databases and config
habakkuk version                  # version, commit, build date and database schema versions
```

`habakkuk update` is built to be boring:

1. It downloads this platform's archive and refuses it unless it matches the release's `SHA256SUMS` (and `SHA256SUMS.sig`, when the binary was built with the release key).
2. It runs the new binary's `habakkuk doctor --upgrade-check` against your `$HABAKKUK_HOME` first. That loads your config and reads every database's schema version without writing anything; if the new release would not cope, the update stops and nothing has changed.
3. It backs up `config.yaml`, `.env` and every database to `$HABAKKUK_HOME/upgrades/` (the three newest backups are kept) and keeps a copy of the current binary.
4. It swaps the binary in one step: a rename on Linux and macOS; on Windows the running `habakkuk.exe` is moved aside and removed the next time Habakkuk starts.

Restart `habakkuk serve` afterwards. If something is wrong, stop it and run `habakkuk update --rollback`: it backs up the current state once more, then restores the previous binary, databases and config.

**Config.** `config.yaml` has a `version:`. When a release changes the format, an older file still loads: it is migrated in memory and Habakkuk says so on start. `habakkuk config migrate` writes the migrated file after saving the old one as `config.yaml.<time>.bak` (`-dry-run` only shows what would change). A key Habakkuk does not know is reported and ignored, so a setting added for a newer release does not stop an older binary after a rollback; `habakkuk doctor` lists them so typos stand out.

**Databases.** Schema changes only ever move forward, each step in a transaction, and run when the new binary first opens a database. An older binary refuses to open a database a newer one has migrated, and says to roll forward (`habakkuk update`) or restore the pre-upgrade backup (`habakkuk update --rollback`, or `habakkuk restore -force FILE`).

**Channels.** `stable` (the default) follows GitHub's latest release; `beta` also takes pre-releases such as `v0.5.0-beta.1`. `HABAKKUK_UPDATE_CHANNEL` overrides the remembered choice for one run.

Container images upgrade by pulling a new tag; the same schema checks apply to the `/data` volume, so back it up (`habakkuk backup -env`) before moving to a new major version.

Next: [Quick start](quickstart.md).
