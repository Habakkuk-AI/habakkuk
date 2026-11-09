---
title: Subscription logins
nav_title: Subscriptions
description: "Use a ChatGPT or GitHub Copilot plan instead of an API key: device-code sign-in, where tokens are kept, and why there is no Claude plan login."
order: 3
---

A provider can use an account sign-in instead of API keys. Two plans are supported: ChatGPT and GitHub Copilot.

```bash
habakkuk auth login chatgpt     # ChatGPT plan: device code, then the Responses API the plan uses
habakkuk auth login copilot     # GitHub Copilot plan: GitHub device code, then Copilot's OpenAI-compatible API
habakkuk auth list
habakkuk auth logout chatgpt
```

Then point a provider at the login:

```yaml
providers:
  chatgpt:
    auth: chatgpt             # type responses is implied
  copilot:
    auth: copilot             # type openai is implied
agents:
  habakkuk:
    model: chatgpt/gpt-5.6-sol
```

`habakkuk setup` finds existing sign-ins and offers them as a model choice.

## How tokens are kept

Tokens are kept in `$HABAKKUK_HOME/auth.json` (owner-only, replaced atomically) and refreshed shortly before they expire, under a file lock so two Habakkuk processes never refresh at once. `habakkuk doctor` reports a provider whose login is missing (`NOT LOGGED IN (run habakkuk auth login chatgpt)`).

Calls through a subscription login count as $0 in [usage and cost](usage-and-limits.md).

> [!NOTE]
> There is no Claude subscription login. Anthropic's terms do not allow third-party apps to use Claude plan credentials, so use an API key.
