---
title: Usage and limits
description: "See tokens and cost by model, agent and session, add prices, and set hard daily, monthly and per-turn spending caps for the install and each agent."
order: 5
---

You can see what your model calls cost, and put a hard cap on that cost. Come here when you want to check your spend or keep an agent from running up a bill.

## Usage and cost

Habakkuk records the tokens of every model call in `usage.db`: input, output, cache read and write. Each record also has the call's provider, model, agent and session.

```bash
hk usage [-days 30] [-agent A] [-session S]         # totals and cost by model and by agent, and spend against the limits
```

In a chat, send `/usage` to see the current conversation. Send `/budget` to see the spend against the limits.

Habakkuk works out costs from a built-in table of list prices. The prices are standard tier, in US dollars per million tokens, with long-prompt tiers where providers have them. A call made through a [subscription login](subscriptions.md) counts as $0. You can add or correct prices in config:

```yaml
pricing:
  llama-4-scout: {input: 0.11, output: 0.34}
  openrouter/anthropic/claude-sonnet-5-5: {input: 2, output: 10, cache_read: 0.1, cache_write: 2.5}
```

## Spending limits

Spending limits are hard caps on what your model calls cost. With them in place, a schedule or a runaway tool loop can't run up a bill overnight. You can set them for the whole install and for each agent. Amounts are in US dollars, written as numbers or strings (`5`, `"5.00"`):

```yaml
timezone: America/Toronto   # days start at 00:00 here, months on the 1st

budget:                     # the whole install
  daily: 5
  monthly: 100
  per_turn: 1.50            # one turn: its tool loop, compaction, title and sub-agents
  unknown_price: allow      # allow | block

agents:
  habakkuk:
    budget: {daily: 2, per_turn: 0.50}   # this agent's own calls
```

### How limits are enforced

- **Every model call is checked first.** Turns, sub-agents, compaction, automatic titles, the approval reviewer, memory review and meeting notes all go through the same router. The router checks the limits before it sends a call. Once a limit is reached, the call isn't sent. A call that's already under way isn't cut off, though. So spend can pass a limit by what the calls running at that moment cost, and then it stops.
- **Spend is kept in memory.** Habakkuk loads it from `usage.db` at start and updates it as each call finishes, so the check costs nothing measurable. The prices are the same ones `hk usage` uses.
- **Who pays.** Each call counts toward the agent that made it, and also toward the install. A sub-agent's calls count toward the sub-agent's own daily and monthly limits. They also count toward the per-turn limit of the turn that delegated to it.
- **Changing limits** doesn't need a restart. Edit them in `config.yaml` and the change applies within seconds.

### What you see

- **A capped turn ends cleanly.** You get a reply that says which limit is used up, when it resets and where to raise it:

  > Paused: the daily budget of $5.00 for agent habakkuk is used up; it resets at 00:00 America/Toronto. Raise it with agents.habakkuk.budget.daily in config.yaml.

  The job finishes, and it isn't retried. A per-turn limit stops only that turn. Your next message starts a fresh allowance.
- **Background work tells you once.** If a scheduled task's agent is out of budget, the task is skipped without starting. Its chat gets one notice per limit and period, and later skips are only logged. A board card is blocked with the reason. Memory review is skipped, then tried again later.
- **Warning at 80%.** When a daily or monthly limit passes 80%, the chat whose call crossed it gets one warning per period. If there's no chat, the warning goes to the log.

### Unknown prices

If a model is missing from the price table, it counts as $0, and the log gets one warning per model. To fix that, add the model under `pricing:`. You can also set `unknown_price: block`. Then, while any limit applies, calls to such models are refused and the next model in the chain is tried.

### Checking spend

There are three places to see spend against the limits. Run `hk usage` for today and this month, for the install and each agent. Send `/budget` in a chat for this conversation's agent and the install. Or call `GET /api/budget` (see [HTTP API](../reference/api.md#get-apibudgetagent)).
