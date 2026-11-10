---
name: data-analysis
description: Explore a spreadsheet or CSV and report what the numbers show. Use when the user shares data.
---

# Data analysis

## Look before you count

1. Open the file and read the first rows. Note the columns, their types and how many rows there are.
2. Work out what one row stands for: an order, a customer, a day.
3. Check the quality: empty cells, duplicates, dates in mixed formats, numbers stored as text, values that cannot be right such as a negative age or a date in the future.
4. Tell the user what you found, and what you will do about it, before going on. Never fix data silently.

Work on a copy. Leave the original file as it is.

## Understand the question

Restate what the user wants to know in one sentence. If they only said "analyse this", propose three questions the data can answer and let them choose.

## Do the work

- Use a tool that computes: a script or a spreadsheet formula. Do not add up long columns in your head.
- Start simple: counts, totals, averages, smallest and largest, change over time, then breakdowns by the obvious groups.
- Compare like with like: the same period, the same units, the same definition.
- For an average, look at the spread too. A few large values can make an average mislead, so give the median as well.
- Keep the steps, so the result can be reproduced and checked.

## Report

1. **The answer**, in one or two sentences, with the number.
2. **The evidence**: a small table. A chart when it shows something the table does not.
3. **What stands out**: at most three things worth knowing that were not asked.
4. **Caveats**: what was missing, what you assumed, how many rows you left out and why.

Round sensibly. Give units and the period on every figure.

## Honesty

- Two things moving together does not show that one causes the other. Say "is associated with" unless there is a real reason to say more.
- A small sample is a weak basis. Give the count beside every percentage.
- If the data cannot answer the question, say so and say what data would.
- Check a surprising result twice before reporting it. Surprises are more often errors than discoveries.
