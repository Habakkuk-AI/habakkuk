---
title: Decision model
description: "Use a fast decision model (TypeSafe's Jev) for Habakkuk's own small choices: which model a message needs, whether a tool call is routine, and whether a chat is worth remembering."
order: 3
---

Habakkuk makes many small yes/no decisions around your conversations. By default it makes them with word rules or a full model call. A decision model answers typed questions with a probability in well under a second, for a fraction of a cent, so these decisions get both cheaper and better.

Habakkuk supports TypeSafe's Jev. It never writes replies; your agents' models still do that.

```yaml
judge:
  model: jev-latest              # default
  timeout_ms: 800                # past this, Habakkuk decides without it
  routing: true                  # which model a message needs (default true)
  approvals: true                # screen tool calls before the reviewer model (default true)
  memory: true                   # skip memory reviews of chats with nothing to keep (default true)
```

Put the key in `$HABAKKUK_HOME/.env` as `TYPESAFE_API_KEY` (or name another variable with `key_env`). Without a key, or when the API is slow or down, everything works as it does without a `judge:` section.

## What it decides

| Decision | Without a decision model | With one |
|---|---|---|
| **Routing.** For agents with a `fast_model`: quick message or real work? | Word rules (build, fix, research, long text, code...) | One question about the message and whether the last turn was real work. Attachments still always go to the main model. See [Routing](routing.md). |
| **Approvals.** For agents with `approvals: reviewer` or `reviewer_then_human`: is this tool call plainly routine? | A model call reviews every call | A call it is at least `approve_above` (0.97) sure is routine, and at most 3% likely to be risky, is allowed without the model call. Anything else goes to the reviewer model as before. |
| **Memory.** Is this chat worth a memory review? | A model call reviews every quiet session | A chat it rates under `skip_memory_below` (0.15) is skipped; the rest are reviewed as before. |

> [!IMPORTANT]
> The decision model can only make approvals faster, never looser. It never denies or asks on its own, never decides for a turn that has read outside content (a web page, an email), and the commands Habakkuk always blocks stay blocked. Raise `approve_above` towards 1 to use it less, or set `approvals: false` to keep every review on the reviewer model.

## Cost

Input costs $0.042 per million tokens and output is free. A routing decision is about 100 to 200 tokens, so a million decisions cost a few dollars. Calls are recorded in `usage.db` under the provider `typesafe`, show up in `habakkuk usage`, and count toward [spending limits](usage-and-limits.md).

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
