---
title: Multi-agent teams
description: "Run several agents in one Habakkuk: route chats to them, switch agents, delegate to sub-agents, give each a home chat, and reach other agents over A2A."
order: 2
---

When one agent isn't enough, you decide which agent answers which chat, and the agents hand work to each other. One Habakkuk process can serve any number of agents. Each agent has its own models, permission mode, workspace, toolsets, `SOUL.md`, memory and skills. See [Agents and identity](agents.md).

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

- **Per channel.** A channel's `agent` setting names the default agent for its conversations.
- **Per conversation or workspace.** `routes` maps a conversation id, or a Slack team id, to an agent. For example: `routes: {T0123456: ops}`.
- **In the chat.** Send `/agents` to list the agents. `/agent NAME` switches this conversation to that agent and starts a fresh session. The switch is kept in `state.db`. `/agent default` goes back to routing.
- **Elsewhere.** From the terminal, use `hk chat -agent A` or `hk run -agent A`. In the web chat API, send `"agent": "ops"`. In the OpenAI-compatible API, use `habakkuk/<agent>` as the model name.

If nothing names an agent, the default is the agent called `habakkuk`, or the only agent you have.

## Delegation

With `delegate`, an agent hands a self-contained task to a sub-agent. The sub-agent is either another configured agent or a fresh copy of the caller. The task becomes a durable job. So if the parent crashes and repeats the call, it reattaches to the child, whether the child is still running or has finished. Several delegate calls in one response run in parallel. Sub-turns have their own worker pool. Sub-agents can't delegate further.

The agent can pass these options:

- `model` (`provider/model`) is the model to try first. The agent's chain comes after it.
- `toolsets` has to be a subset of the parent's.
- `output_schema` is a JSON Schema that the child's final answer must match. The parent gets only the JSON.
- `worktree: true` gives the child its own `git worktree` of the repository that holds the parent's workspace. The worktree is on branch `habakkuk/<job>`, and the child's changes are committed there.
- `background: true` returns at once. When the child ends, the parent gets a new turn with the report, so the parent's answer reaches the chat. `/subagents` lists the children that are running, and `/subagents stop ID` cancels one.

> [!NOTE]
> Sub-agents run unattended, so calls that need approval are denied unless the agent has an approval reviewer. A delegated task inherits its parent's taint and origin.

For the details, see [Architecture: delegation](../reference/architecture.md#delegation).

## The board

Delegation waits for an answer. When the work should outlive the conversation, an agent creates a card on the [task board](board.md) and assigns it to another agent. The assignee works on the card in its own session, and the work carries on across restarts. The result comes back to the creator's conversation.

## Teams and home chats

Several agents can work as a team, with each one in its own chat. Say marketing, sales and support are WhatsApp agents. Each has one `whatsappagent` channel, made under Settings > Agents. An orchestrator answers you on Slack:

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

Ask the orchestrator on Slack "what's up with the campaign?" and it delegates the question to marketing. Marketing's answer goes back to the orchestrator, which sums it up for you on Slack. A short version also lands in your Marketing chat on WhatsApp. Each agent's chat keeps the history of its own work.

`home` sets the agent's home chat. You write it like a `hk send` target, as `CHANNEL:CONVERSATION` or `CHANNEL:CONVERSATION:THREAD`, and the channel must be one you've configured. On a `whatsappagent` channel, `creator` stands for you. Send each agent one message first so it knows your id.

`report_home` decides what gets posted there automatically:

| `report_home` | Posted to the home chat |
|---|---|
| `results` (the default once `home` is set) | When the agent finishes work that did not start in its home chat: "**Done:** the task", its final reply cut to about 1,200 characters, and who got the full result |
| `all` | Also "Working on: the task" when such work starts |
| `off` | Nothing automatic |

Work that didn't start in the home chat means a task another agent delegated, a board card, a scheduled run, an A2A task, or a message in another channel. Some things are never reported:

- a turn from the home chat itself (or from any thread of it, when the home names no thread);
- turns you type in a local client (terminal, editor, API), because you already see the answer;
- compactions and `[SILENT]` replies;
- a task an agent delegated to a copy of itself, because its parent turn reports in its place, so a chain of sub-agents posts once;
- a run that fails, because whoever asked gets the error.

A report is an ordinary queued delivery, and its id is derived from the turn. So a turn that's retried doesn't post twice. A report never starts a turn, which means agents can't loop. Sometimes another agent answers the home chat. That agent sees the report on your next message there, so a reply such as "yes" answers the question that was actually asked. See [Sessions](sessions.md#messages-from-elsewhere-in-the-same-chat).

### Posting to the home chat

An agent with a home also gets the `message_home` tool. The agent uses it to post progress, questions or fuller results to its home chat in the middle of delegated or scheduled work. Like a reply, the tool needs no approval. It refuses to run in the home chat itself.

> [!IMPORTANT]
> Once a turn has read outside content (web pages, search results, network tools, an unvetted message), `message_home` sends only a short status. The status is at most 280 characters, with no links and no code. This stops text planted in a page from reaching you looking like your agent's own words. The details go in the final reply.

## Agents on other servers

Habakkuk speaks the Agent2Agent (A2A) protocol in both directions. It can serve its agents to other A2A clients. Agents with the `a2a` toolset can also discover and message agents elsewhere. See [Integrations: Agent2Agent](../tools/integrations.md#agent2agent).
