---
title: Updating
description: "Move to a new release with habakkuk update, follow the beta channel, roll back, and how config and database changes are handled."
order: 3
---

```bash
habakkuk update -check            # is there a newer release on your channel?
habakkuk update                   # install it
habakkuk update -channel beta     # follow pre-releases too (remembered; -channel stable goes back)
habakkuk update --rollback        # undo the last update: binary, databases and config
habakkuk version                  # version, commit, build date and database schema versions
```

Restart `habakkuk serve` after an update.

## What an update does

`habakkuk update` is built to be boring.

1. It downloads this platform's archive and refuses it unless it matches the release's `SHA256SUMS` (and `SHA256SUMS.sig`, when the binary was built with the release key).
2. It runs the new binary's `habakkuk doctor --upgrade-check` against your `$HABAKKUK_HOME` first. That loads your config and reads every database's schema version without writing anything. If the new release would not cope, the update stops and nothing has changed. `-skip-upgrade-check` installs anyway.
3. It backs up `config.yaml`, `.env` and every database to `$HABAKKUK_HOME/upgrades/` (the three newest backups are kept) and keeps a copy of the current binary.
4. It swaps the binary in one step: a rename on Linux and macOS. On Windows the running `habakkuk.exe` is moved aside and removed the next time Habakkuk starts.

## Rolling back

If something is wrong, stop `habakkuk serve` and run:

```bash
habakkuk update --rollback
```

It backs up the current state once more, then restores the previous binary, databases and config.

## Config changes

`config.yaml` has a `version:`. When a release changes the format, an older file still loads: it is migrated in memory and Habakkuk says so on start. `habakkuk config migrate` writes the migrated file after saving the old one as `config.yaml.<time>.bak` (`-dry-run` only shows what would change).

A key Habakkuk does not know is reported and ignored, so a setting added for a newer release does not stop an older binary after a rollback. `habakkuk doctor` lists such keys so typos stand out.

## Database changes

Schema changes only move forward, each step in a transaction, and run when the new binary first opens a database. An older binary refuses to open a database a newer one has migrated, and says to roll forward (`habakkuk update`) or restore the pre-upgrade backup (`habakkuk update --rollback`, or `habakkuk restore -force FILE`).

## Release channels

`stable` (the default) follows GitHub's latest release. `beta` also takes pre-releases such as `v0.5.0-beta.1`. `HABAKKUK_UPDATE_CHANNEL` overrides the remembered choice for one run.

## Containers

Container images upgrade by pulling a new tag. The same schema checks apply to the `/data` volume, so back it up (`habakkuk backup -env`) before moving to a new major version.

> [!TIP]
> Habakkuk checks for updates only when you run `habakkuk update`. It never phones home on its own.
