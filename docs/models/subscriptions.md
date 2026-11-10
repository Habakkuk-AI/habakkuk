---
title: Subscription logins
nav_title: Subscriptions
description: "Sign in with a ChatGPT or GitHub Copilot plan by device code instead of an API key, see where tokens are kept, and why there's no Claude plan login."
order: 4
---

If you have a ChatGPT or GitHub Copilot plan, a provider can use that account sign-in instead of API keys. Those are the two plans Habakkuk supports. Start by signing in:

```bash
hk auth login chatgpt           # ChatGPT plan: device code, then the Responses API the plan uses
hk auth login copilot           # GitHub Copilot plan: GitHub device code, then Copilot's OpenAI-compatible API
hk auth list
hk auth logout chatgpt
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

If you've already signed in, `hk setup` finds the sign-in and offers it as a model choice.

## How tokens are kept

Habakkuk keeps your tokens in `$HABAKKUK_HOME/auth.json`. The file is owner-only, and it's replaced atomically. Habakkuk refreshes the tokens shortly before they expire. It holds a file lock while it does, so two Habakkuk processes never refresh at once. If a provider's login is missing, `hk doctor` reports it (`NOT LOGGED IN (run hk auth login chatgpt)`).

A call made through a subscription login counts as $0 in [usage and cost](usage-and-limits.md).

> [!NOTE]
> There's no Claude subscription login. Anthropic's terms don't allow third-party apps to use Claude plan credentials, so use an API key instead.
