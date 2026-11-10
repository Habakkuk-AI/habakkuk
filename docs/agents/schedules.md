---
title: Schedules
description: "Run prompts and scripts on a timer: cron expressions, intervals, one-off times, at most one run per slot, scripts that skip the model, and where results go."
order: 5
---

A schedule runs an agent on a timer. This page shows how to create one, how its runs behave, and how to schedule a script in place of a prompt. Schedules need `hk serve` running, because it polls them every 15 seconds.

## Creating a schedule

You can ask in a chat, for example "every weekday at 8, summarise my inbox". The agent creates the schedule with the `schedule_create` tool. Results go to the chat where the schedule was created.

You can also manage schedules from the terminal:

```bash
hk schedules                                        # list
hk schedules add -agent habakkuk -name standup -when "0 9 * * 1-5" -prompt "Post today's board" -to telegram:123456789
hk schedules pause|resume|rm|run ID
```

`-when` takes one of these forms:

| Form | Example |
|---|---|
| A cron expression | `"0 9 * * 1-5"` (9:00 on weekdays) |
| An interval | `"@every 30m"` |
| One time | `"at 2026-12-24 18:00"`, or with a zone: `"at 2026-12-24T23:00:00Z"` |

Times are read in the config's `timezone`. If you haven't set one, that's the server's time zone. `schedules add` prints the first run with its zone. When that zone isn't UTC, it prints the UTC time too:

```
sch_4f1c scheduled; first run Mon 2026-11-09 17:57 America/Toronto (22:57 UTC)
The time was read in America/Toronto, the config timezone. For another zone, give an offset: at 2026-11-09T17:57:00Z is UTC.
```

`-to` takes a `CHANNEL:CHAT[:THREAD]` target, the same as `hk send`. To queue an extra run right now, use `hk schedules run ID`.

`hk schedules` shows the last result of each schedule. That's `started` while a run is going. Afterwards it's `done`, `failed: <reason>`, or a script's own status. A one-off schedule that has run shows as `ran once`.

## How runs work

- **At most once per slot.** A slot is one time the schedule comes due. For each due slot, the scheduler queues a job whose ID is the schedule plus the slot time. That's why a slot runs at most once, even when several processes are polling or there's a crash in between.
- **No late runs.** If a slot is more than an hour late because Habakkuk was down, it's skipped. Habakkuk doesn't run it late.
- **Fresh sessions.** Each run gets a fresh session. Whatever the run sends to a chat is still shown to the agent that answers that chat, on your next message there. So you can ask about it. See [Sessions](sessions.md#messages-from-elsewhere-in-the-same-chat).
- **Approvals.** When a call needs approval, Habakkuk asks you in the chat the schedule delivers to and waits up to 15 minutes. Answer **Always allow** and later runs of a schedule you made go ahead without asking. While a run is still going, the next slot is skipped. A schedule with no delivery chat has nobody to ask, so the call is denied, unless the agent has an approval reviewer (`approvals: reviewer` or `reviewer_then_human`) or a saved rule covers it. A run that has read outside content always asks, and so does every run of a schedule you didn't make yourself (one a board card, sub-agent, notice or tainted turn created). See [Approvals in scheduled runs](approvals.md#approvals-in-scheduled-runs).
- **Budgets.** If the agent is out of budget, the scheduled task is skipped before it starts. Its chat gets one notice per limit and period. See [Spending limits](../models/usage-and-limits.md#spending-limits).

## Script schedules

A script schedule runs a command where an ordinary schedule runs a prompt. Use one for frequent jobs, such as syncing a ledger, checking an inbox or sending reminders, where a model call on every run would be slow and costly.

```bash
hk schedules add -agent habakkuk -name inbox -when "@every 10m" \
  -run ./check-inbox.sh -on-output deliver -timeout 2m -to telegram:123456789
```

- **Who can add one.** Only the owner can, through the CLI. `schedule_create` has no command field and refuses one. That keeps an agent from turning scheduling into unattended shell access.
- **Running.** The command runs with `/bin/sh -c` (PowerShell on Windows) in the workdir. You set the workdir with `-workdir`, and the default is the agent's workspace. The command gets the environment plus `$HABAKKUK_HOME/.env`, along with `HABAKKUK_AGENT`, `HABAKKUK_SCHEDULE_ID` and `HABAKKUK_SCHEDULE_NAME`. If the command hits the timeout (default 120s), everything it started is killed.
- **Output.** Empty stdout means the script has nothing to say. So does a line that's exactly `[SILENT]`. Otherwise `-on-output` decides what happens:

| `-on-output` | What happens |
|---|---|
| `deliver` | stdout is sent to the chat |
| `agent` | a turn starts with the schedule's prompt and the output appended |
| `silent` | nothing is sent |

- **Failures.** A non-zero exit or a timeout is recorded, and the run isn't retried. The third failure in a row sends one notice to the chat. The first success after that tells the chat the script works again.
- **No overlap.** If a slot comes due while the previous run is still going, it's skipped.

> [!NOTE]
> You can't declare script schedules in `config.yaml` yet, and there's no command to edit one in place. To change one, remove it and add it again.

For the details, see [Architecture: schedules](../reference/architecture.md#schedules).
