---
title: Usage and limits
description: "Tokens and cost by model, agent and session, adding prices, and hard daily, monthly and per-turn spending caps for the install and each agent."
order: 4
---

## Usage and cost

Every model call's tokens (input, output, cache read and write) are recorded with its provider, model, agent and session in `usage.db`.

```bash
habakkuk usage [-days 30] [-agent A] [-session S]   # totals and cost by model and by agent, and spend against the limits
```

In a chat, `/usage` shows the current conversation and `/budget` the spend against the limits.

Costs come from a built-in table of list prices (standard tier, US dollars per million tokens), with long-prompt tiers where providers have them. Calls through a [subscription login](subscriptions.md) count as $0. Add or correct prices in config:

```yaml
pricing:
  llama-4-scout: {input: 0.11, output: 0.34}
  openrouter/anthropic/claude-sonnet-5-5: {input: 2, output: 10, cache_read: 0.1, cache_write: 2.5}
```

## Spending limits

Hard caps on what model calls cost, so a schedule or a runaway tool loop cannot run up a bill overnight. Set them for the whole install and per agent, in US dollars (numbers or strings: `5`, `"5.00"`):

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

- **Every model call is checked first**: turns, sub-agents, compaction, automatic titles, the approval reviewer, memory review and meeting notes all go through the same router, which asks the limits before sending. Once a limit is reached the call is not sent. A call already under way is not cut off, so spend can pass a limit by what the calls running at that moment cost, and then stops.
- **Spend is kept in memory**, loaded from `usage.db` at start and updated as each call finishes, so the check costs nothing measurable. Prices are the ones `habakkuk usage` uses.
- **Who pays.** A call counts toward the agent that made it and toward the install. A sub-agent's calls count toward the sub-agent's daily and monthly limits, and toward the per-turn limit of the turn that delegated to it.
- **Changing limits** in `config.yaml` applies within seconds, without a restart.

### What you see

- **A capped turn ends cleanly** with a reply saying which limit is used up, when it resets and where to raise it:

  > Paused: the daily budget of $5.00 for agent habakkuk is used up; it resets at 00:00 America/Toronto. Raise it with agents.habakkuk.budget.daily in config.yaml.

  The job finishes; it is not retried. A per-turn limit stops only that turn; the next message starts a fresh allowance.
- **Background work tells you once.** A scheduled task whose agent is out of budget is skipped without starting, and its chat gets one notice per limit and period; later skips are only logged. A board card is blocked with the reason. Memory review is skipped and tried again later.
- **Warning at 80%.** When a daily or monthly limit passes 80%, the chat whose call crossed it gets one warning per period (the log, when there is no chat).

### Unknown prices

A model missing from the price table counts as $0, with one warning per model in the log; add it under `pricing:`. With `unknown_price: block`, calls to such models are refused while any limit applies, and the next model in the chain is tried.

### Checking spend

See spend against the limits with `habakkuk usage` (today and this month for the install and each agent), `/budget` in a chat (this conversation's agent and the install), or `GET /api/budget` ([HTTP API](../reference/api.md#get-apibudgetagent)).
