---
name: debugging
description: Find the cause of a bug step by step and fix the cause. Use when something fails or gives a wrong result.
---

# Debugging

Guessing is the slow way. Narrow it down.

## 1. Get the facts

- The exact error message, in full, and where it appears.
- What was expected, and what happened.
- When it started, and what changed just before: code, config, data, a dependency, the machine.
- Whether it happens every time.

Read the error and the stack trace properly. The cause is very often named in the first lines, and people skip them.

## 2. Reproduce it

Find the smallest set of steps that makes it fail, and run them. A bug you cannot reproduce you cannot confirm fixed. If it only fails sometimes, find what differs between a good run and a bad one: input, timing, order, environment.

## 3. Narrow it down

- Halve the search. Does the problem exist at this point? Check in the middle, then in the middle of the half that fails.
- Compare with something that works: an earlier commit, another input, another machine. `git bisect` finds the commit that broke it.
- Add logging or use a debugger at the boundary between what is right and what is wrong. Look at the real values, not what you assume they are.
- Change one thing at a time, and undo changes that did not help.

## 4. Explain it

Before you fix anything, state the cause in a sentence: "X happens because Y, when Z." If you cannot, you have not found it yet. Check the explanation against every symptom. If one does not fit, keep looking.

## 5. Fix the cause

- Fix where the fault begins, not where it shows.
- Make the smallest change that removes the cause.
- Write a test that fails before the fix and passes after.
- Search for the same mistake elsewhere.
- Run the whole test suite, not only the new test.

## 6. Report

Say what was wrong, why, what you changed, and how you checked it. Note anything you found on the way that is still broken.

## When stuck

- Explain the problem aloud, line by line. The gap usually shows.
- Question the thing you are sure of. It is often there.
- Check the dull causes: the wrong file, a stale build, an old process still running, a cache, the wrong branch, a typo in a name.
- After an hour without progress, stop and tell the user what you have ruled out and what you would try next.

Never hide a failure to make it go away: no catching and ignoring the error, no deleting the failing test, no retry loop around a bug.
