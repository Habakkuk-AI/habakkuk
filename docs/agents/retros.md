---
title: Retros
description: "Retrospectives: an agent looks back over its own work, finds what went wrong and why from Habakkuk's records, proposes changes, applies the safe ones, and checks next time whether they helped."
order: 9
---

A retro is an agent looking back over a period of its own work. It reads what Habakkuk recorded, finds what went well and what went badly, works out likely causes, and proposes concrete changes. Some changes can be applied on their own; the rest wait for you. The next retro is shown the last one's changes and says whether they helped.

Retros run on a timer when you turn them on, or whenever you ask:

```bash
habakkuk retro                      # the default agent's last 7 days, now
habakkuk retro -agent coder -days 3
habakkuk retro -dry-run             # print the evidence and the plan; change nothing
```

In a chat, `/retro` does the same for the conversation's agent and replies with the summary.

## What a retro looks at

The evidence comes from Habakkuk's own stores, gathered without a model. For one agent and period it collects:

- **Turns and sessions:** how many, how many failed, the median and 95th percentile time to finish a turn, and the slowest few with the message that started them.
- **Jobs that went wrong:** failed, retried, interrupted (the worker died) or canceled, with a sample of each error.
- **Tool errors**, counted by tool, with examples.
- **Approvals:** calls the reviewer approved or denied, calls the reviewer would not decide with no human to ask, calls you declined or did not answer in time, and calls nobody was there to approve (a scheduled run, for example).
- **Model fallbacks:** answers from a model other than the agent's own, and fallbacks and credential cool-downs from `habakkuk.log` with their reasons (`auth`, `rate_limit`, ...).
- **Cost by model.**
- **Schedules** with script errors, failure streaks, runs skipped by a spending limit, missed slots, failed turns, or approvals nobody could give.
- **Stalled work:** board cards and delegated tasks with no progress for three days.
- **Deliveries** to the agent's chats that failed or kept retrying.
- **Signs you were not happy:** short messages like "no, that's not what I asked", the same request repeated, `/undo` of a turn's file changes, and `/undo-memory`.
- **Memory and skill changes** made in the period.

Every item cites where it came from: `job:ID`, `session:ID`, `card:ID`, `schedule:ID`, `change:ID`, `checkpoint:ID` or `log:TIME`. Sizes are capped and text passes through the same secret redaction as `habakkuk.log`.

The model is also shown the agent's `SOUL.md`, its skills, its schedules, and the previous retro's actions with what became of them.

## What the model is asked

The model answers with a summary, what went well, what went badly (each with its likely cause), proposed actions, and a follow-up on the last retro's actions: helped, did not help, or unclear.

Habakkuk keeps only what holds up. A point that cites no evidence id from the input is dropped. An action with an unknown kind, no change, no reason, a bad skill name or anything that looks like a secret is dropped. Up to 8 actions are kept. The report lists what was left out and why.

Each action has a kind:

| Kind | What it changes | Applied on its own? |
|---|---|---|
| `skill` | Creates or replaces one of the agent's own skills | With `apply: safe` |
| `memory` | Creates or replaces a memory note | With `apply: safe` |
| `soul` | Adds to `SOUL.md`, or replaces one exact passage | Never |
| `config` | A change to `config.yaml` | Never, not even when approved |
| `schedule` | Pauses a schedule, or changes its spec or prompt | Never |
| `ask` | A question only you can answer | Never |

## Configuration

```yaml
retro:
  every: weekly          # off (default) | daily | weekly | a cron expression
  at: "fri 17:00"        # local time for daily ("17:00") and weekly; default fri 17:00
  agents: []             # default: every agent that had turns in the period
  model: ""              # default: the agent's own models; a strong model is right here
  apply: propose         # propose: nothing changes until you approve; safe: skill and memory changes at once
  deliver: ""            # a CHANNEL:CHAT target, none, or empty for the agent's home chat
```

- **When.** `habakkuk serve` checks once a minute. A retro covers the time since the slot before (a week for `weekly`). A slot more than a day late (the server was down) is skipped, not run late. A cron expression may run at most once an hour.
- **Agents.** With `agents` empty, every agent with turns in the period gets one.
- **Model.** `model` is tried first, then the agent's own chain.
- **Times** are in the config's `timezone`.

Without a `retro:` section nothing runs on a timer, but `habakkuk retro` and `/retro` still work.

## Approving proposals

The summary and the report number the proposals. Answer them in a chat or in the terminal:

```bash
habakkuk retro list [-agent A]          # past retros, newest first
habakkuk retro show ID                  # the full report; ID may be latest
habakkuk retro apply ID 1 3             # apply proposals 1 and 3
habakkuk retro reject ID 2
```

```text
/retro apply 1 3
/retro reject 2
```

`/retro` works on the conversation agent's latest retro. Only the owner can use it; guests are refused.

What applying does:

- **Skill, memory and `SOUL.md` changes** are written through the [memory history](memory.md#memory-history-and-undo). Each is a recorded change with an id, so `habakkuk memory restore ID` or `/undo-memory ID` undoes it. The change notes which retro and action made it.
- **Pausing a schedule** is done for you; `habakkuk schedules resume ID` turns it back on. Any other schedule change is spelled out for you to make.
- **Config changes are never made for you.** Applying one marks it `approved` and prints the exact edit to make in `config.yaml`. A wrong config can stop Habakkuk from starting, and many keys change what agents may do (permissions, approvals, channels, budgets). That stays a decision you make in the file, checked by `habakkuk doctor`.
- **Questions** are marked `approved` when you say yes.

Each action has a status: `proposed`, `applied`, `approved` (you said yes to something only you can do), `rejected` or `failed` (with the reason). Applying an action twice changes nothing the second time. A rejected action can still be applied later. An applied one cannot be rejected; undo it through the memory history instead.

## What is never automatic

Even with `apply: safe`:

- `SOUL.md`, config, schedule changes and questions always wait for you.
- Pinned notes and skills are left alone; the action stays proposed.
- Notes a retro writes on its own are marked untrusted, because the evidence includes tool output and messages from outside. `habakkuk memory trust SLUG` vouches for one. Notes you approve are not marked.
- Every change it makes is recorded in the memory history as made by `retro`, and can be undone.

## Reports and delivery

Every retro writes `agents/<agent>/retros/YYYY-MM-DD.md` (named by the period's last day, with `-2`, `-3` for more than one a day). It holds the summary, what went well and badly, each proposal with its exact change, reason, expected effect, risk, evidence and status, the follow-up on the last retro, and the evidence itself. Approving or rejecting updates the file.

A scheduled retro also sends a short summary to `deliver`, or the agent's home chat: what went wrong, what changed, and the numbered proposals that need a yes. If nothing notable happened it sends nothing. Retros you run with `habakkuk retro` or `/retro` are not delivered; you see the result where you asked.

An example summary:

```text
Retro for coder (Nov 2 to Nov 9)

Two engineering tasks stalled after their pull requests were opened, and
replies to one chat failed 7 times.

What went wrong
- PR #42 was opened on Nov 3 and never reviewed
- Replies to tg:555 were retried 7 times: chat not found

Needs a yes
1. skill pr-follow-up: PRs sat with no reviewer
2. config agents.coder.home: the home chat no longer exists

Reply /retro apply N or /retro reject N.
Full report: agents/coder/retros/2026-11-09.md
```

## How it is stored

A retro is a durable job whose id comes from the agent and the period, so the same period is queued once however often the scheduler looks. The job gathers the evidence, asks the model once, and stores the plan and its actions in `memory.db`. It then works through the actions, recording progress after each. After a crash it reuses the stored plan, without asking the model again, and carries on where it stopped.

A retro uses the agent's model budget like any other call; one skipped by a spending limit is not retried.

The dashboard API has `GET /api/retros/{agent}` and `GET /api/retros/{agent}/{id}` ([HTTP API](../reference/api.md#retros)).
