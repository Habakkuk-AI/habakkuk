---
title: Backups and updates
description: "Back up and restore $HABAKKUK_HOME while Habakkuk runs, move to another machine, update and roll back, and how retention keeps storage small."
order: 4
---

## Backups

```bash
habakkuk backup -o habakkuk.tar.gz       # config, agents, skills and database snapshots
habakkuk backup -env -workspaces         # also .env, WhatsApp credentials and workspaces inside the home
habakkuk restore habakkuk.tar.gz         # unpack a backup into $HABAKKUK_HOME
```

- `habakkuk backup [-o FILE] [-env] [-workspaces]` takes consistent database snapshots while Habakkuk runs, so there is no need to stop it.
- `-env` adds `.env` and the WhatsApp credential databases.
- `-workspaces` adds agent workspaces kept under `agents/<name>/workspace`. A workspace elsewhere (such as `~/habakkuk-workspace`, which `habakkuk setup` uses) is not included; copy it yourself.
- `habakkuk restore [-force] FILE` refuses to overwrite an existing config without `-force`. Stop `habakkuk serve` first.

> [!WARNING]
> A backup made with `-env` holds your keys and can act as your linked WhatsApp device. Keep it private.

### Moving to another machine

1. On the old machine: `habakkuk backup -env -workspaces -o habakkuk.tar.gz`, then stop `serve`.
2. Install Habakkuk on the new machine ([Installation](../get-started/installation.md)).
3. Copy the archive over (and any workspace outside `$HABAKKUK_HOME`), and run `habakkuk restore habakkuk.tar.gz`.
4. Run `habakkuk doctor`, then start `serve` there. Only one machine should run `serve` for the same channels.

## Updates

```bash
habakkuk update -check                   # is there a newer release?
habakkuk update                          # install it (checksum verified, checked against your home, after a backup)
habakkuk update --rollback               # undo the last update
```

Restart `serve` after updating. Container users pull the new image instead. [Updating](../get-started/updating.md) explains each step, release channels, and how config and database changes are handled.

## Retention

```yaml
retention:
  days: 90      # transcripts and finished jobs older than this are pruned; 0 disables
memory:
  history_days: 90   # memory change history; -1 keeps all
```

`habakkuk serve` prunes old transcripts and finished jobs on its maintenance pass. Memory notes, skills and the user profile are files, and are never pruned by retention.
