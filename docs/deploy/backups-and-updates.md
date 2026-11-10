---
title: Backups and updates
description: "Back up and restore $HABAKKUK_HOME while Habakkuk runs, move it to another machine, update or roll back, and see how retention keeps storage small."
order: 4
---

Everything Habakkuk keeps is in `$HABAKKUK_HOME`, so that's what you back up, restore and move to another machine. Updates, rollbacks and retention (which keeps storage small) are covered here too.

## Backups

```bash
habakkuk backup -o habakkuk.tar.gz       # config, agents, skills and database snapshots
habakkuk backup -env -workspaces         # also .env, WhatsApp credentials and workspaces inside the home
habakkuk restore habakkuk.tar.gz         # unpack a backup into $HABAKKUK_HOME
```

- `habakkuk backup [-o FILE] [-env] [-workspaces]` takes consistent snapshots of the databases while Habakkuk runs, so you don't need to stop it.
- `-env` also includes `.env` and the WhatsApp credential databases.
- `-workspaces` also includes the agent workspaces kept under `agents/<name>/workspace`. A workspace somewhere else isn't included, so copy it yourself. One example is `~/habakkuk-workspace`, which `habakkuk setup` uses.
- `habakkuk restore [-force] FILE` won't overwrite an existing config unless you pass `-force`. Stop `habakkuk serve` before you restore.

> [!WARNING]
> A backup made with `-env` contains your keys and can act as your linked WhatsApp device. Keep it private.

### Moving to another machine

1. On the old machine, run `habakkuk backup -env -workspaces -o habakkuk.tar.gz`. Then stop `serve`.
2. Install Habakkuk on the new machine. [Installation](../get-started/installation.md) shows you how.
3. Copy the archive to the new machine, along with any workspace outside `$HABAKKUK_HOME`. Then run `habakkuk restore habakkuk.tar.gz`.
4. Run `habakkuk doctor`, then start `serve` on the new machine. Only one machine should run `serve` for the same channels.

## Updates

```bash
habakkuk update -check                   # is there a newer release?
habakkuk update                          # install it (checksum verified, checked against your home, after a backup)
habakkuk update --rollback               # undo the last update
```

Restart `serve` after you update. If you run Habakkuk in a container, pull the new image instead. [Updating](../get-started/updating.md) walks you through each step and the release channels, and explains how config and database changes are handled.

## Retention

```yaml
retention:
  days: 90      # transcripts and finished jobs older than this are pruned; 0 disables
memory:
  history_days: 90   # memory change history; -1 keeps all
```

`habakkuk serve` prunes (deletes) old transcripts and finished jobs during its maintenance pass. Memory notes, skills and the user profile are files, and retention never prunes them.
