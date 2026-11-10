---
title: Multi-agent teams
description: "Run several agents in one Habakkuk: route conversations to them, switch agents in a chat, delegate to sub-agents, give each a home chat, and talk to agents elsewhere over A2A."
order: 2
---

One Habakkuk process serves any number of agents. Each has its own models, permission mode, workspace, toolsets, `SOUL.md`, memory and skills (see [Agents and identity](agents.md)).

```yaml
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
    permission: workspace
  research:
    model: anthropic/claude-sonnet-5-5
    fallbacks: [deepseek/deepseek-v4-pro]
    toolsets: [core, memory, browser]
  ops:
    model: anthropic/claude-sonnet-5-5
    approvals: reviewer_then_human
```

## Who answers a conversation

- **Per channel.** A channel's `agent` is the default for its conversations.
- **Per conversation or workspace.** `routes` maps a conversation id (or a Slack team id) to an agent: `routes: {T0123456: ops}`.
- **In the chat.** `/agents` lists the agents. `/agent NAME` switches this conversation (kept in `state.db`) and starts a fresh session. `/agent default` goes back to routing.
- **Elsewhere.** `habakkuk chat -agent A`, `habakkuk run -agent A`, `"agent": "ops"` in the web chat API, and `habakkuk/<agent>` as the model name in the OpenAI-compatible API.

When nothing names an agent, the default is the agent called `habakkuk`, or the only agent.

## Delegation

`delegate` hands a self-contained task to a sub-agent: another configured agent, or a fresh copy of the caller. The task becomes a durable job, so a parent that crashed and repeats the call reattaches to the running or finished child. Several delegate calls in one response run in parallel, sub-turns have their own worker pool, and sub-agents cannot delegate further.

Options the agent can pass:

- `model` (`provider/model`) is tried first, the agent's chain after it.
- `toolsets` must be a subset of the parent's.
- `output_schema` is a JSON Schema the child's final answer must match; the parent gets just the JSON.
- `worktree: true` gives the child its own `git worktree` of the repository holding the parent's workspace, on branch `habakkuk/<job>`, and commits its changes there.
- `background: true` returns at once. When the child ends, the parent gets a new turn with the report, so its answer reaches the chat. `/subagents` lists running children and `/subagents stop ID` cancels one.

> [!NOTE]
> Sub-agents run unattended: calls that need approval are denied unless the agent has an approval reviewer. A delegated task inherits its parent's taint and origin.

[Architecture: delegation](../reference/architecture.md#delegation) has the details.

## The board

Delegation waits for an answer. For work that should outlive the conversation, an agent creates a card on the [task board](board.md) assigned to another agent. The assignee works on it in its own session, across restarts, and the result comes back to the creator's conversation.

## Teams and home chats

Several agents can work as a team, each in its own chat. Say marketing, sales and support are WhatsApp agents (one `whatsappagent` channel each, made under Settings > Agents) and an orchestrator answers you on Slack:

```yaml
agents:
  orchestrator:
    model: anthropic/claude-opus-5-5     # the default toolsets include delegate and board
  marketing:
    model: anthropic/claude-sonnet-5-5
    home: wa-marketing:creator           # its own WhatsApp chat
  sales:
    model: anthropic/claude-sonnet-5-5
    home: wa-sales:creator
    report_home: all                     # also "Working on: ..." when work starts
  support:
    model: anthropic/claude-sonnet-5-5
    home: wa-support:creator
channels:
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN
    bot_tokens_env: [SLACK_BOT_TOKEN]
    agent: orchestrator
  wa-marketing:
    type: whatsappagent
    token_env: WA_MARKETING_KEY
    agent: marketing
  wa-sales:
    type: whatsappagent
    token_env: WA_SALES_KEY
    agent: sales
  wa-support:
    type: whatsappagent
    token_env: WA_SUPPORT_KEY
    agent: support
```

Ask the orchestrator on Slack "what's up with the campaign?" and it delegates to marketing. Marketing's answer goes back to the orchestrator, which sums it up on Slack, and a short version lands in your Marketing chat on WhatsApp. Each agent's chat keeps the history of its own work.

`home` is the agent's home chat, written like a `habakkuk send` target: `CHANNEL:CONVERSATION` or `CHANNEL:CONVERSATION:THREAD`, with a configured channel. On a `whatsappagent` channel `creator` stands for you; send each agent one message first so it knows your id.

`report_home` decides what is posted there on its own:

| `report_home` | Posted to the home chat |
|---|---|
| `results` (the default once `home` is set) | When the agent finishes work that did not start in its home chat: "**Done:** the task", its final reply cut to about 1,200 characters, and who got the full result |
| `all` | Also "Working on: the task" when such work starts |
| `off` | Nothing automatic |

Work that did not start in the home chat is a task another agent delegated, a board card, a scheduled run, an A2A task, or a message in another channel. These are never reported:

- a turn from the home chat itself (any thread of it, when the home names no thread);
- turns typed in a local client (terminal, editor, API), whose answer you already see;
- compactions and `[SILENT]` replies;
- a task an agent delegated to a copy of itself: its parent turn reports instead, so a chain of sub-agents posts once;
- a run that fails: whoever asked gets the error.

Reports are ordinary queued deliveries with ids derived from the turn, so a retried turn never posts twice. A report never starts a turn, so agents cannot loop. When the home chat is answered by another agent, that agent sees the report on your next message there, so a reply such as "yes" answers the question that was actually asked. See [Sessions](sessions.md#messages-from-elsewhere-in-the-same-chat).

### Posting to the home chat

An agent with a home also gets the `message_home` tool, to post progress, questions or fuller results to its home chat in the middle of delegated or scheduled work. It needs no approval, like a reply, and refuses in the home chat itself.

> [!IMPORTANT]
> Once a turn has read outside content (web pages, search results, network tools, an unvetted message), `message_home` sends only a short status: at most 280 characters, with no links and no code. Text planted in a page cannot reach you looking like your agent's own words; the details go in the final reply instead.

## Agents on other servers

Habakkuk speaks the Agent2Agent protocol both ways: it can serve its agents to other A2A clients, and agents with the `a2a` toolset can discover and message agents elsewhere. See [Integrations: Agent2Agent](../tools/integrations.md#agent2agent).
