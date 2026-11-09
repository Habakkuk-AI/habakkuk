---
title: MCP
description: "Give agents tools from any Model Context Protocol server (stdio or Streamable HTTP, with OAuth sign-in), add servers from the built-in catalog, and serve Habakkuk itself over MCP."
order: 7
---

## Using MCP servers

Agents can use tools from any MCP server. Each server's tools form the toolset `mcp:<name>`, and each tool is named `mcp__<name>__<tool>`:

```yaml
agents:
  habakkuk:
    toolsets: [core, "mcp:github", "mcp:search"]
mcp_servers:
  github:
    command: github-mcp-server                 # stdio; gets PATH, HOME and env below, nothing else
    args: [stdio]
    env: {GITHUB_PERSONAL_ACCESS_TOKEN: GITHUB_TOKEN}   # child variable: variable in env or .env
    risk: network                              # read | write | network | exec (default network)
    approve: [create_pull_request]             # always ask before these
  search:
    url: https://mcp.example.com/mcp           # Streamable HTTP
    headers: {Authorization: SEARCH_MCP_AUTH}  # header: variable holding its value
    allow: [web_search]                        # expose only these tools
    timeout: 60s                               # per call (default 120s)
  linear:
    url: https://mcp.linear.app/mcp
    auth: oauth                                # sign in once: habakkuk mcp login linear
```

`habakkuk mcp` connects to each server and lists its status and tools. A server that is down never blocks startup: its tools report it as unavailable and recover once it comes back.

- **Risk.** A server declares one `risk` for its tools. `network`, the default, is allowed without asking in `workspace` mode. Tools listed in `approve` are treated as `exec` and always need approval below `full`. `allow` exposes only the named tools.
- **Isolation.** A stdio server gets `PATH`, `HOME` and the variables in its `env` map, never Habakkuk's whole environment. Secrets in `env` and `headers` are named, not stored, and `habakkuk doctor` reports any that are missing.
- **Outside content.** Results from MCP tools taint the turn, like web pages.
- **Many tools.** When MCP servers add more tools than `tool_search_over` (default 60), the agent finds them with [tool search](index.md#tool-search) instead of seeing them all.

[Architecture: MCP](../reference/architecture.md#mcp) covers naming, reconnects and results.

## OAuth sign-in

Hosted servers that use the MCP authorization flow take `auth: oauth`:

```bash
habakkuk mcp login linear       # opens the sign-in page; -device uses the device flow
habakkuk mcp logout linear
```

`habakkuk mcp login NAME` registers Habakkuk with the server's authorization server, opens the sign-in page (or, without a browser, uses the device flow when offered) and keeps the tokens in `auth.json`, refreshing them as needed. `client_id`, `client_secret_env`, `scopes` and `callback_port` are there for servers that need a registered app.

## The catalog

```bash
habakkuk mcp catalog [QUERY]          # well-known MCP servers Habakkuk can add
habakkuk mcp add NAME [param=value]   # add one to config.yaml
```

The catalog is built into the binary: GitHub, GitLab, Linear, Notion, Slack, Google Drive, Stripe, Sentry, Supabase, PostgreSQL, SQLite, the filesystem, fetch, git, Brave Search, Exa, Tavily, Firecrawl, Playwright, Puppeteer, Figma, Atlassian, Cloudflare, Vercel, Railway, MongoDB, Hugging Face and more, each with how it runs, the variables it needs and the programs it expects on `PATH`. Only servers published by their vendor or by the MCP project are listed; the MCP project's archived reference servers are marked.

`habakkuk mcp add NAME [param=value ...]` appends the server to `mcp_servers` in `config.yaml`:

- The file is edited as text, so its comments and layout stay as they were (a flow-style `mcp_servers: {}` is rewritten through the YAML tree instead). The result must load, and the previous file is kept as `config.yaml.bak`.
- Entries with placeholders take them as parameters: `habakkuk mcp add filesystem dir=~/Documents`, `habakkuk mcp add sqlite db=~/data/app.db`. `-name` stores it under another name.
- Afterwards Habakkuk says which variables to put in `.env`, which programs it needs on `PATH`, and to add `mcp:NAME` to an agent's toolsets.

[Plugins](plugins.md) can also bring MCP servers.

## Habakkuk as an MCP server

Other MCP clients can talk to Habakkuk's agents:

```bash
habakkuk mcp serve          # MCP on stdio
```

```yaml
mcp_server:
  http: true                # also serve Streamable HTTP at /mcp (needs the http section; same token)
  allow_send: false         # true exposes habakkuk_send, which posts a message to any configured chat
```

The tools are `habakkuk_ask(agent?, message, session?)`, `habakkuk_sessions_list`, `habakkuk_session_read`, `habakkuk_board_create`, `habakkuk_board_list` and `habakkuk_schedules_list`, plus `habakkuk_send(account, conversation, text)` when `allow_send` is on. Every ask runs as an ordinary turn job.
