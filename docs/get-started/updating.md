---
title: Updating
description: "Move to a new release with habakkuk update, follow the beta channel, roll back, and see how config and database changes are handled."
order: 3
---

You can move Habakkuk to a new release, follow the beta channel or roll back from the command line. Further down is what an update does to your config and databases. These are the commands:

```bash
habakkuk update -check            # is there a newer release on your channel?
habakkuk update                   # install it
habakkuk update -channel beta     # follow pre-releases too (remembered; -channel stable goes back)
habakkuk update --rollback        # undo the last update: binary, databases and config
habakkuk version                  # version, commit, build date and database schema versions
```

After an update, restart `habakkuk serve`.

## What an update does

`habakkuk update` is built to be boring. It works in four steps.

1. It downloads the archive for your platform and checks it against the release's `SHA256SUMS`. When the binary was built with the release key, it checks `SHA256SUMS.sig` too. It refuses an archive that doesn't match.
2. Before it installs anything, it runs the new binary's `habakkuk doctor --upgrade-check` against your `$HABAKKUK_HOME`. That check loads your config and reads the schema version of every database (the version of its layout), and it writes nothing. If the new release can't cope with what it finds, the update stops and nothing has changed. To install anyway, pass `-skip-upgrade-check`.
3. It backs up `config.yaml`, `.env` and every database to `$HABAKKUK_HOME/upgrades/`, where the three newest backups are kept. It also keeps a copy of the current binary.
4. It swaps the binary in one step. On Linux and macOS, that step is a rename. On Windows, the running `habakkuk.exe` is moved aside, then removed the next time Habakkuk starts.

## Rolling back

If something's wrong after an update, stop `habakkuk serve`. Then run:

```bash
habakkuk update --rollback
```

The rollback backs up the current state once more. Then it restores the previous binary, databases and config.

## Config changes

`config.yaml` has a `version:` setting. When a release changes the format, your older file still loads. Habakkuk migrates it in memory and tells you so when it starts. To write the migrated file, run `habakkuk config migrate`. It saves the old file as `config.yaml.<time>.bak` first. Add `-dry-run` and it only shows what would change.

If Habakkuk finds a key it doesn't know, it reports the key and ignores it. That way, a setting added for a newer release doesn't stop an older binary after a rollback. `habakkuk doctor` lists these keys, so typos stand out.

## Database changes

Schema changes only move forward. Each step runs in a transaction, when the new binary first opens a database. An older binary refuses to open a database that a newer one has migrated. It tells you to roll forward with `habakkuk update`, or to restore the pre-upgrade backup with `habakkuk update --rollback` or `habakkuk restore -force FILE`.

## Release channels

There are two channels. `stable` is the default, and it follows GitHub's latest release. `beta` takes pre-releases as well, such as `v0.5.0-beta.1`. To override the remembered choice for one run, set `HABAKKUK_UPDATE_CHANNEL`.

## Containers

To upgrade a container, pull a new image tag. The same schema checks apply to the `/data` volume, so back it up with `habakkuk backup -env` before you move to a new major version.

> [!TIP]
> Habakkuk looks for updates only when you run `habakkuk update`. On its own, it never phones home.
