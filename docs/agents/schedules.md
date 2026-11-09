---
title: Schedules
description: "Scheduled prompts and scripts: cron expressions, intervals and one-off times, at most one run per slot, script schedules that skip the model, and where results go."
order: 5
---

Schedules run an agent on a timer. They need `habakkuk serve` running: it polls the schedules every 15 seconds.

## Creating a schedule

Ask in a chat ("every weekday at 8, summarise my inbox"), and the agent creates the schedule with the `schedule_create` tool. Results go to the chat the schedule was created from.

Or manage them from the terminal:

```bash
habakkuk schedules                                  # list
habakkuk schedules add -agent habakkuk -name standup -when "0 9 * * 1-5" -prompt "Post today's board" -to telegram:123456789
habakkuk schedules pause|resume|rm|run ID
```

`-when` takes:

| Form | Example |
|---|---|
| A cron expression | `"0 9 * * 1-5"` (9:00 on weekdays) |
| An interval | `"@every 30m"` |
| One time | `"at 2026-12-24 18:00"` |

Times are in the config's `timezone` (default the server's). `-to` takes a `CHANNEL:CHAT[:THREAD]` target, as `habakkuk send` does. `habakkuk schedules run ID` queues an extra run now.

## How runs work

- **At most once per slot.** For each due slot the scheduler enqueues a job whose ID is the schedule plus the slot time, so a slot runs at most once even with several processes polling or a crash in between.
- **No late runs.** A slot more than an hour late (Habakkuk was down) is skipped, not run late.
- **Fresh sessions.** Each run is a fresh session.
- **Unattended.** Anything that needs approval is denied unless the agent has an approval reviewer (`approvals: reviewer` or `reviewer_then_human`; see [Permissions and approvals](approvals.md#approvals)).
- **Budgets.** A scheduled task whose agent is out of budget is skipped without starting, and its chat gets one notice per limit and period. See [Spending limits](../models/usage-and-limits.md#spending-limits).

## Script schedules

Script schedules run a command instead of a prompt. Use them for frequent jobs (sync a ledger, check an inbox, send reminders) where a model call per run would be slow and costly.

```bash
habakkuk schedules add -agent habakkuk -name inbox -when "@every 10m" \
  -run ./check-inbox.sh -on-output deliver -timeout 2m -to telegram:123456789
```

- **Who can add one.** Only the owner, through the CLI. `schedule_create` has no command field and refuses one, so an agent cannot turn scheduling into unattended shell access.
- **Running.** The command runs with `/bin/sh -c` (PowerShell on Windows) in the workdir (`-workdir`, default the agent's workspace). It gets the environment plus `$HABAKKUK_HOME/.env` and `HABAKKUK_AGENT`, `HABAKKUK_SCHEDULE_ID` and `HABAKKUK_SCHEDULE_NAME`. A timeout (default 120s) kills everything it started.
- **Output.** Empty stdout, or a line that is exactly `[SILENT]`, means nothing to say. Otherwise `-on-output` decides:

| `-on-output` | What happens |
|---|---|
| `deliver` | stdout is sent to the chat |
| `agent` | a turn starts with the schedule's prompt and the output appended |
| `silent` | nothing is sent |

- **Failures.** A non-zero exit or a timeout is recorded and not retried. The third failure in a row sends one notice to the chat, and the first success after it says the script works again.
- **No overlap.** A slot that comes due while the previous run is still going is skipped.

> [!NOTE]
> Script schedules cannot yet be declared in `config.yaml`, and there is no command to edit one in place: remove it and add it again.

[Architecture: schedules](../reference/architecture.md#schedules) has the details.
