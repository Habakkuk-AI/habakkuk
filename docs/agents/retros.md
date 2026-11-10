---
title: Retros
description: "In a retro, an agent reviews its own work, finds what went wrong and why, proposes changes, applies the safe ones, and checks later whether they helped."
order: 9
---

A retro (short for retrospective) is when an agent looks back over a period of its own work. It reads what Habakkuk recorded and finds what went well and what went badly. It works out the likely causes and proposes concrete changes. Some of those changes can be applied on their own, and the rest wait for you. The next retro sees the changes from the last one and says whether they helped.

Once you turn them on, retros run on a timer. You can also run one yourself at any time:

```bash
hk retro                            # the default agent's last 7 days, now
hk retro -agent coder -days 3
hk retro -dry-run                   # print the evidence and the plan; change nothing
```

In a chat, send `/retro` to do the same for the conversation's agent. It replies with the summary.

## What a retro looks at

The evidence comes from Habakkuk's own stores, and Habakkuk gathers it without a model. For one agent and one period, it collects:

- **Turns and sessions:** how many there were, how many failed, the median and 95th percentile time to finish a turn, and the slowest few turns with the message that started each.
- **Jobs that went wrong:** jobs that failed, were retried, were interrupted (the worker died) or were canceled, with a sample of each error.
- **Tool errors**, counted by tool, with examples.
- **Approvals:** calls the reviewer approved or denied, calls the reviewer wouldn't decide when there was no human to ask, calls you declined or didn't answer in time, and calls that nobody was there to approve (in a scheduled run, for example).
- **Model fallbacks:** answers that came from a model other than the agent's own, plus the fallbacks and credential cool-downs in `habakkuk.log`, with their reasons (`auth`, `rate_limit`, ...).
- **Cost by model.**
- **Schedules** that had script errors, failure streaks, runs skipped by a spending limit, missed slots, failed turns, or approvals nobody could give.
- **Stalled work:** board cards and delegated tasks that have made no progress for three days.
- **Deliveries** to the agent's chats that failed or kept retrying.
- **Signs you weren't happy:** short messages like "no, that's not what I asked", the same request sent again, an `/undo` of a turn's file changes, and `/undo-memory`.
- **Memory and skill changes** made during the period.

Every item cites where it came from, as `job:ID`, `session:ID`, `card:ID`, `schedule:ID`, `change:ID`, `checkpoint:ID` or `log:TIME`. Sizes are capped. Text goes through the same secret redaction as `habakkuk.log`.

The model also sees the agent's `SOUL.md`, its skills and its schedules. It sees the previous retro's actions too, with what became of them.

## What the model is asked

The model answers with a summary, what went well, what went badly (each with its likely cause) and proposed actions. It also follows up on the last retro's actions, saying whether each one helped, didn't help or is unclear.

Habakkuk keeps only what holds up. It drops a point that cites no evidence id from the input. It drops an action that has an unknown kind, no change, no reason, a bad skill name or anything that looks like a secret. It keeps up to 8 actions. The report lists what was left out and why.

Every action has a kind:

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

- **When.** `hk serve` checks once a minute. A retro covers the time since the previous slot, which is a week for `weekly`. If a slot is more than a day late because the server was down, it's skipped. Habakkuk doesn't run it late. A cron expression may run at most once an hour.
- **Agents.** If `agents` is empty, every agent that had turns in the period gets a retro.
- **Model.** Habakkuk tries `model` first, then the agent's own chain.
- **Times** use the `timezone` from the config.

If there's no `retro:` section, nothing runs on a timer. `hk retro` and `/retro` still work.

## Approving proposals

The proposals are numbered in both the summary and the report. You answer them in a chat or in the terminal:

```bash
hk retro list [-agent A]                # past retros, newest first
hk retro show ID                        # the full report; ID may be latest
hk retro apply ID 1 3                   # apply proposals 1 and 3
hk retro reject ID 2
```

```text
/retro apply 1 3
/retro reject 2
```

`/retro` works on the latest retro of the conversation's agent. Only the owner can use it. Guests are refused.

Here's what applying does:

- **Skill, memory and `SOUL.md` changes** are written through the [memory history](memory.md#memory-history-and-undo). Each one is recorded as a change with an id, so you can undo it with `hk memory restore ID` or `/undo-memory ID`. The change notes which retro and which action made it.
- **Pausing a schedule** is done for you. To turn the schedule back on, run `hk schedules resume ID`. Any other schedule change is spelled out so you can make it yourself.
- **Config changes are never made for you.** When you apply one, Habakkuk marks it `approved` and prints the exact edit to make in `config.yaml`. A wrong config can stop Habakkuk from starting, and many keys change what agents may do (permissions, approvals, channels, budgets). So the edit stays a decision you make in the file, and `hk doctor` checks it.
- **Questions** are marked `approved` when you say yes.

Each action has a status. It's one of `proposed`, `applied`, `approved` (you said yes to something only you can do), `rejected` or `failed` (with the reason). If you apply an action twice, the second time changes nothing. You can still apply a rejected action later. You can't reject an applied one, but you can undo it through the memory history.

## What is never automatic

These rules hold even with `apply: safe`:

- Changes to `SOUL.md`, config and schedules always wait for you. So do questions.
- Pinned notes and skills are left alone, and the action stays proposed.
- A note that a retro writes on its own is marked untrusted, because the evidence includes tool output and messages from outside. Run `hk memory trust SLUG` to vouch for one. Notes you approve aren't marked.
- Every change the retro makes is recorded in the memory history as made by `retro`, and you can undo it.

## Reports and delivery

Every retro writes a report to `agents/<agent>/retros/YYYY-MM-DD.md`. The file is named for the last day of the period, with `-2`, `-3` added when there's more than one a day. The report holds the summary, what went well and what went badly, and the follow-up on the last retro. It lists each proposal with its exact change, reason, expected effect, risk, evidence and status. It also holds the evidence itself. When you approve or reject a proposal, the file is updated.

A scheduled retro also sends a short summary to `deliver`, or to the agent's home chat. The summary says what went wrong, what changed, and which numbered proposals need a yes. If nothing notable happened, it sends nothing. A retro you run with `hk retro` or `/retro` isn't delivered, because you see the result where you asked.

Here's an example summary:

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

A retro is a durable job, and its id comes from the agent and the period. So the same period is queued once, however often the scheduler looks. The job gathers the evidence, asks the model once, and stores the plan and its actions in `memory.db`. Then it works through the actions and records its progress after each one. After a crash, it reuses the stored plan, so it doesn't ask the model again. It carries on where it stopped.

A retro uses the agent's model budget like any other call. If a spending limit skips a retro, that retro isn't retried.

The dashboard API has `GET /api/retros/{agent}` and `GET /api/retros/{agent}/{id}`. See [HTTP API](../reference/api.md#retros).
