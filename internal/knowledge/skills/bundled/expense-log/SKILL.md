---
name: expense-log
description: Record receipts in a simple log and total spending by category. Use for receipts or what did I spend.
---

# Expense log

## Where it lives

Keep one log file in the workspace, `expenses.csv`, unless the user already has a sheet or an accounting tool you can write to. Columns:

`date, merchant, amount, currency, category, paid_with, note, source`

Create it with that header the first time. Never rewrite old rows. Add new ones at the end.

## Recording one

From a photo, a PDF, an email or a typed line, read:

- the date on the receipt, not today's date,
- the merchant's name as printed,
- the total actually paid, with tax, and the currency,
- how it was paid, when it shows.

Choose a category from the user's own list. If they have none, start with: food, transport, home, bills, health, work, leisure, other. Ask once when a merchant could be two categories, then remember the answer for that merchant.

Put the file name or message the receipt came from in `source`. Mark anything you could not read clearly with a question mark in `note`, and tell the user which field it was.

Before adding, check for the same date, merchant and amount. If it is there, say so and do not add it twice.

## Answering questions

For "what did I spend":

1. Take the period asked for. Default to this month.
2. Total by category, largest first, then the overall total.
3. Keep currencies apart. Do not convert unless asked, and give the rate and its date when you do.
4. Do the sum twice. The category totals must add up to the overall total.

Mention what stands out: a category well above last month, a charge that appears twice, a subscription the user may have forgotten.

## Limits

This is a record, not tax or accounting advice. For what can be claimed, point the user to their accountant or the tax office's own guidance.
