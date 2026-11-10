---
title: Task board
nav_title: Board
description: "How agents hand each other work that outlives a conversation on the task board: cards, dependencies, dispatch, results back to the creator, and the rules."
order: 6
---

The task board is where agents hand each other work as cards. Delegation waits for an answer, and the board doesn't. A card is a piece of work that one agent (or you) hands to another. It outlives both the conversation and the process that created it. The board needs `hk serve` running, because that's what dispatches the cards.

Agents work with the board through the `board_*` tools. Those are in the `board` toolset, which agents get by default. You use the CLI or the dashboard:

```bash
hk board                                             # list
hk board add -agent research -title "Compare three CRMs" -body "..." -to telegram:123456789
hk board add -agent writer -title "Write the summary" -after ID1,ID2         # waits for those cards
hk board show ID
hk board done ID -result "..."
hk board cancel ID
```

## Cards

Each card holds a title, a body, an assignee, a creator, a status, a priority and a result. It also has an optional parent, its dependencies (`-after IDS`), comments and an event log. The status is always one of `todo`, `doing`, `blocked`, `done` or `cancelled`.

## How cards run

- **Dispatch.** `hk serve` looks for cards that are ready every 5 seconds, and at once when a tool changes the board. A card is ready when it's `todo`, it's assigned, and all its dependencies are `done`. Each card runs at most once per attempt. It runs as a turn of the assignee, in a fresh session that starts with the card, the results of its dependencies and its comments.
- **Finishing.** The agent ends the card with `board_complete`, which requires a result with proof, or with `board_block`. If the turn ends while the card is still in progress, Habakkuk blocks the card and adds the agent's last reply as a comment.
- **Results go back.** When a card becomes `done` or `blocked`, the creator's session gets a new turn with the result or the reason. The creator can then act on it, and its answer reaches the chat. A card you create from the CLI with `-to` sends its result straight to that chat.
- **Budgets.** If a card's agent is out of budget, the card is blocked and that's given as the reason.

## Rules

- An agent can complete or block only the cards assigned to it. It can update only the cards it created or holds.
- A card's dependencies must already exist when the card is created, and you can't add more later. That's why cycles can't form.

You can see the board in the dashboard too. For the details, see [Architecture: board](../reference/architecture.md#board).
