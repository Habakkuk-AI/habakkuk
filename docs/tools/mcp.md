---
title: MCP
description: "Give agents tools from any MCP server over stdio or Streamable HTTP, sign in with OAuth, add servers from the built-in catalog, and serve Habakkuk over MCP."
order: 7
---

MCP is short for Model Context Protocol. An MCP server is a program that gives an agent more tools. This page shows you how to connect servers, sign in to hosted ones, add servers from the built-in catalog, and let other MCP clients use Habakkuk.

## Using MCP servers

An agent can use tools from any MCP server. The tools of one server make up the toolset `mcp:<name>`. Each tool is named `mcp__<name>__<tool>`. For example:

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
    auth: oauth                                # sign in once: hk mcp login linear
```

Run `hk mcp` to connect to each server and list its status and tools. A server that's down doesn't hold up startup. Its tools report it as unavailable, and they recover once it comes back.

- **Risk.** Each server has one `risk` that covers all its tools. The default is `network`, which `workspace` mode allows without asking. Any tool you list in `approve` is treated as `exec`, so below `full` it always needs approval. With `allow`, only the tools you name are exposed.
- **Isolation.** A stdio server gets `PATH`, `HOME` and the variables in its `env` map. It doesn't get the rest of Habakkuk's environment. For secrets, `env` and `headers` hold the name of a variable, so the secret itself isn't stored. A server that takes its key on the command line or in its address can name a variable in an argument or the URL as `${NAME}`, for example `args: ["--token", "${SHOP_TOKEN}"]` or `url: https://mcp.example.com${SHOP_PATH}`. `hk doctor` reports any that are missing.
- **Outside content.** Results from MCP tools taint the turn, the same way web pages do.
- **Many tools.** If MCP servers add more tools than `tool_search_over` (default 60), the agent doesn't see them all at once. It finds them with [tool search](index.md#tool-search).

For naming, reconnects and results, see [Architecture: MCP](../reference/architecture.md#mcp).

## OAuth sign-in

Some hosted servers use the MCP authorization flow. Give those `auth: oauth` in the config, then sign in:

```bash
hk mcp login linear             # opens the sign-in page; -device uses the device flow
hk mcp logout linear
```

`hk mcp login NAME` first registers Habakkuk with the server's authorization server. Then it opens the sign-in page. If there's no browser, it uses the device flow when the server offers it. It keeps the tokens in `auth.json` and refreshes them as needed. Some servers need a registered app. The settings `client_id`, `client_secret_env`, `scopes` and `callback_port` are there for those.

## The catalog

```bash
hk mcp catalog [QUERY]                # well-known MCP servers Habakkuk can add
hk mcp add NAME [param=value]         # add one to config.yaml
```

The catalog is a list of well-known MCP servers, and it's built into the binary. It has GitHub, GitLab, Linear, Notion, Slack, Google Drive, Stripe, Sentry, Supabase, PostgreSQL, SQLite, the filesystem, fetch, git, Brave Search, Exa, Tavily, Firecrawl, Playwright, Puppeteer, Figma, Atlassian, Cloudflare, Vercel, Railway, MongoDB, Hugging Face and more. Each entry says how the server runs, which variables it needs and which programs it expects on `PATH`. The catalog lists only servers published by their vendor or by the MCP project. The MCP project's archived reference servers are marked.

`hk mcp add NAME [param=value ...]` adds the server to the end of `mcp_servers` in `config.yaml`. Here's how it behaves:

- Habakkuk edits the file as text, so its comments and layout stay as they were. The exception is a flow-style `mcp_servers: {}`, which is rewritten through the YAML tree. The result has to load, and the previous file is kept as `config.yaml.bak`.
- Some entries have placeholders, and you fill them in as parameters. For example, `hk mcp add filesystem dir=~/Documents` or `hk mcp add sqlite db=~/data/app.db`. Use `-name` to store the server under another name.
- When it's done, Habakkuk tells you which variables to put in `.env` and which programs the server needs on `PATH`. It also tells you to add `mcp:NAME` to an agent's toolsets.

A [plugin](plugins.md) can bring MCP servers too.

## Habakkuk as an MCP server

Habakkuk can be an MCP server itself. That lets other MCP clients talk to your agents:

```bash
hk mcp serve                # MCP on stdio
```

```yaml
mcp_server:
  http: true                # also serve Streamable HTTP at /mcp (needs the http section; same token)
  allow_send: false         # true exposes habakkuk_send, which posts a message to any configured chat
```

Clients get the tools `habakkuk_ask(agent?, message, session?)`, `habakkuk_sessions_list`, `habakkuk_session_read`, `habakkuk_board_create`, `habakkuk_board_list` and `habakkuk_schedules_list`. When `allow_send` is on, they also get `habakkuk_send(account, conversation, text)`. Every ask runs as an ordinary turn job.
