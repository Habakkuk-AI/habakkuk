---
title: Schedules and the task board
description: "Scheduled prompts and scripts with at-most-once slots, and the task board agents use to hand each other work that outlives a conversation."
order: 10
---

Both need `habakkuk serve` running: it polls the schedules and dispatches board cards.

## Schedules

Agents create schedules with the `schedule_create` tool when you ask ("every weekday at 8, summarise my inbox"), and you can manage them from the terminal:

```bash
habakkuk schedules                                  # list
habakkuk schedules add -agent habakkuk -name standup -when "0 9 * * 1-5" -prompt "Post today's board" -to telegram:123456789
habakkuk schedules pause|resume|rm|run ID
```

`-when` takes a cron expression, `@every 30m`, or a one-off `at YYYY-MM-DD HH:MM`, interpreted in the config `timezone`. The scheduler polls every 15 seconds. For each due slot it enqueues a job whose ID is the schedule plus the slot time, so a slot runs at most once even with several processes polling or a crash in between. A slot more than an hour late (Habakkuk was down) is skipped, not run late. Each run is a fresh session; results go to the chat the schedule was created from. `habakkuk schedules run ID` queues an extra run now.

Scheduled runs are unattended, so anything that needs approval is denied unless the agent has an approval reviewer (`approvals: reviewer` or `reviewer_then_human`; see [Tools and approvals](tools-and-approvals.md#approvals)).

### Script schedules

Script schedules run a command instead of a prompt, for frequent jobs (sync a ledger, check an inbox, send reminders) where a model call per run would be slow and costly:

```bash
habakkuk schedules add -agent habakkuk -name inbox -when "@every 10m" \
  -run ./check-inbox.sh -on-output deliver -timeout 2m -to telegram:123456789
```

- **Who can add one.** Only the owner, through the CLI. `schedule_create` has no command field and refuses one, so an agent cannot turn scheduling into unattended shell access.
- **Running.** `/bin/sh -c` in the workdir (default the agent's workspace), with the environment plus `$HABAKKUK_HOME/.env` and `HABAKKUK_AGENT`, `HABAKKUK_SCHEDULE_ID`, `HABAKKUK_SCHEDULE_NAME`. A timeout (default 120s) kills everything it started.
- **Output.** Empty stdout, or a line that is exactly `[SILENT]`, means nothing to say. Otherwise `deliver` sends stdout to the chat, `agent` starts a turn with the schedule's prompt and the output appended, and `silent` sends nothing.
- **Failures.** A non-zero exit or a timeout is recorded and not retried. The third failure in a row sends one notice to the chat, and the first success after it says the script works again.
- **No overlap.** A slot that comes due while the previous run is still going is skipped.

Script schedules cannot yet be declared in `config.yaml`, and there is no command to edit one in place (remove and add again).

## The task board

Delegation waits for an answer; the board does not. A card is work one agent (or you, with `habakkuk board add`) hands to another, and it outlives the conversation and the process that created it.

```bash
habakkuk board                                       # list
habakkuk board add -agent research -title "Compare three CRMs" -body "..." -to telegram:123456789
habakkuk board show ID
habakkuk board done ID -result "..."
habakkuk board cancel ID
```

- **Cards** have a title, body, assignee, creator, status (`todo`, `doing`, `blocked`, `done`, `cancelled`), priority, result, optional parent, dependencies (`-after IDS`), comments and an event log.
- **Dispatch.** `habakkuk serve` looks every 5 seconds, and at once when a tool changes the board, for cards that are `todo`, assigned, and whose dependencies are all `done`. Each runs at most once per attempt, as a turn of the assignee in a fresh session that starts with the card, its dependencies' results and its comments.
- **Finishing.** The agent ends the card with `board_complete` (a result with proof is required) or `board_block`. If the turn ends with the card still in progress, the card is blocked with the agent's last reply as a comment.
- **Results go back.** When a card becomes `done` or `blocked`, the creator's session gets a new turn with the result or the reason, so the creator can act on it and its answer reaches the chat. Cards created from the CLI with `-to` send the result straight to that chat.
- **Rules.** Agents complete or block only cards assigned to them, and update only cards they created or hold. Dependencies must exist when a card is created and cannot be added later, so cycles cannot form.

The dashboard shows the board too. [Architecture: board](../reference/architecture.md#board) and [schedules](../reference/architecture.md#schedules) have the details.
