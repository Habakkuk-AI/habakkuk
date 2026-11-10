---
name: write-tests
description: Write tests that check behaviour and would fail if the code were wrong. Use when tests are asked for or missing.
---

# Writing tests

A test earns its place by failing when the code is wrong. Coverage numbers do not show that.

## Before writing

1. Read the code and say in a sentence what it promises to do.
2. Find how this project tests: the framework, where test files live, how they are named and how they run. Copy the nearest existing test's style.
3. Run the existing tests first. Know they pass before you add to them.

## What to test

List the cases before writing any:

- **The main path**: the ordinary input and the expected result.
- **Edges**: empty, one, many, the largest allowed, zero, negative, missing, duplicates, text with spaces and non-English characters.
- **Errors**: bad input, a dependency that fails, a timeout. Check the right error comes back, not only that something does.
- **Rules**: every "must" and "never" in the requirements or the comments.
- **Past bugs**: one test for each bug fixed, named after what went wrong.

Test what the code does, through its public face. Tests that reach into internals break on every tidy-up and catch nothing.

## How to write one

- One behaviour per test. The name says it: `rejects_an_order_with_no_items`.
- Three parts: set up, act, check. Keep each short.
- Check the result precisely. "Does not crash" is not a check.
- Keep tests independent: no shared state, no reliance on the order they run in.
- Make them the same on every run. Fix the clock, the random seed and the time zone. Do not call real networks.
- Fake only what is slow, costly or outside your control. A test that fakes everything tests the fakes.
- Use realistic but invented data. Never real names, addresses, keys or customer records.

## Prove the test works

Run it and see it pass. Then break the code on purpose, or revert the fix, and see it fail. A test that has never failed has never been tested.

## Finish

Run the whole suite. Report what you covered, what you chose not to and why, and any behaviour you found that looks wrong. Do not change the code to make a test pass, or weaken a test to fit the code, without telling the user which one you believe.
