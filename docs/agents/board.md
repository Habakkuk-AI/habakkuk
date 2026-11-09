---
title: Task board
nav_title: Board
description: "The task board agents use to hand each other work that outlives a conversation: cards, dependencies, dispatch, results back to the creator, and the rules."
order: 6
---

Delegation waits for an answer; the board does not. A card is work one agent (or you) hands to another. It outlives the conversation and the process that created it. The board needs `habakkuk serve` running: it dispatches the cards.

Agents use the `board_*` tools, in the `board` toolset they get by default. You use the CLI or the dashboard:

```bash
habakkuk board                                       # list
habakkuk board add -agent research -title "Compare three CRMs" -body "..." -to telegram:123456789
habakkuk board add -agent writer -title "Write the summary" -after ID1,ID2   # waits for those cards
habakkuk board show ID
habakkuk board done ID -result "..."
habakkuk board cancel ID
```

## Cards

A card has a title, body, assignee, creator, status, priority, result, optional parent, dependencies (`-after IDS`), comments and an event log. Its status is one of `todo`, `doing`, `blocked`, `done` or `cancelled`.

## How cards run

- **Dispatch.** `habakkuk serve` looks every 5 seconds, and at once when a tool changes the board, for cards that are `todo`, assigned, and whose dependencies are all `done`. Each runs at most once per attempt, as a turn of the assignee in a fresh session that starts with the card, its dependencies' results and its comments.
- **Finishing.** The agent ends the card with `board_complete` (a result with proof is required) or `board_block`. If the turn ends with the card still in progress, the card is blocked with the agent's last reply as a comment.
- **Results go back.** When a card becomes `done` or `blocked`, the creator's session gets a new turn with the result or the reason, so the creator can act on it and its answer reaches the chat. Cards created from the CLI with `-to` send the result straight to that chat.
- **Budgets.** A card whose agent is out of budget is blocked with the reason.

## Rules

- Agents complete or block only cards assigned to them, and update only cards they created or hold.
- Dependencies must exist when a card is created and cannot be added later, so cycles cannot form.

The dashboard shows the board too. [Architecture: board](../reference/architecture.md#board) has the details.
