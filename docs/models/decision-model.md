---
title: Decision model
description: "Let a fast decision model (TypeSafe's Jev) make Habakkuk's small choices: model routing, routine tool calls and which chats are worth remembering."
order: 3
---

Habakkuk makes many small yes/no decisions around your conversations. By default it makes them with word rules or a full model call. A decision model answers a typed question with a probability. It does that in well under a second, for a fraction of a cent, so these decisions get both cheaper and better. This page shows how to turn one on and what it decides.

The decision model Habakkuk supports is TypeSafe's Jev. It never writes replies. Your agents' models still do that.

```yaml
judge:
  model: jev-latest              # default
  timeout_ms: 800                # past this, Habakkuk decides without it
  routing: true                  # which model a message needs (default true)
  approvals: true                # screen tool calls before the reviewer model (default true)
  memory: true                   # skip memory reviews of chats with nothing to keep (default true)
```

Put your key in `$HABAKKUK_HOME/.env` as `TYPESAFE_API_KEY`. To use another variable, name it with `key_env`. If there's no key, or the API is slow or down, everything works the way it does without a `judge:` section.

## What it decides

| Decision | Without a decision model | With one |
|---|---|---|
| **Routing.** For agents with a `fast_model`: quick message or real work? | Word rules (build, fix, research, long text, code...) | One question about the message and whether the last turn was real work. Attachments still always go to the main model. See [Routing](routing.md). |
| **Approvals.** For agents with `approvals: reviewer` or `reviewer_then_human`: is this tool call plainly routine? | A model call reviews every call | A call it is at least `approve_above` (0.97) sure is routine, and at most 3% likely to be risky, is allowed without the model call. Anything else goes to the reviewer model as before. |
| **Memory.** Is this chat worth a memory review? | A model call reviews every quiet session | A chat it rates under `skip_memory_below` (0.15) is skipped; the rest are reviewed as before. |

> [!IMPORTANT]
> The decision model can make approvals faster. It can't make them looser. It never denies or asks on its own. It never decides for a turn that has read outside content (a web page, an email). The commands Habakkuk always blocks stay blocked. To use it less, raise `approve_above` towards 1. To keep every review on the reviewer model, set `approvals: false`.

## Cost

Input costs $0.042 per million tokens, and output is free. A routing decision uses about 100 to 200 tokens, so a million decisions cost a few dollars. Habakkuk records these calls in `usage.db` under the provider `typesafe`. You'll see them in `habakkuk usage`, and they count toward your [spending limits](usage-and-limits.md).

## Settings

| Key | Default | |
|---|---|---|
| `model` | `jev-latest` | A model id, for example `jev-1.14.0` to pin a version |
| `base_url` | `https://api.typesafe.ai/v1` | The API, or a gateway that serves the same API |
| `key_env` | `TYPESAFE_API_KEY` | The variable in `.env` that holds the key |
| `timeout_ms` | 800 | How long a decision may take (100 to 10000) |
| `routing`, `approvals`, `memory` | `true` | Which decisions it makes |
| `approve_above` | 0.97 | How sure it must be that a tool call is routine (0.9 to below 1) |
| `skip_memory_below` | 0.15 | Below this, a chat's memory review is skipped (0 to 0.5) |
