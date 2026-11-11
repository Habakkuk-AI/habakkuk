---
name: pr-description
description: Write a commit message or pull request description that says what changed and why. Use when committing or opening a PR.
---

# Commit messages and pull request descriptions

The reader is someone in six months trying to work out why this line exists. Write for them.

## Look at the change first

Read the full diff and the commits on the branch. Describe what is there, not what you remember doing. Check for anything that should not be included: debug output, unrelated edits, secrets.

Follow the project's own conventions. Look at recent history for the format of titles, and for any template.

## Commit message

- **Title**: one line, about 50 to 70 characters, in the imperative: "Fix total when the cart is empty". If the project uses prefixes such as `fix:` or `feat:`, use them.
- **Blank line.**
- **Body**: why the change was needed and what it does, in a few plain sentences. Wrap the lines. Leave it out for a change that is obvious from the title.

One logical change per commit. If the title needs "and", it is probably two commits.

## Pull request description

1. **Title**: what the change does, as a user or maintainer would say it.
2. **Why**: the problem, in two or three sentences. Link the issue.
3. **What changed**: the approach, and the main parts touched. Not a file list. The diff has that.
4. **How it was checked**: the tests you ran, with the commands, and what you tried by hand. Say plainly what you did not test.
5. **Notes for the reviewer**: where to look first, the decision you were least sure of, anything deliberately left for later.
6. **Risk and rollout**: migrations, config changes, anything that cannot be undone, and how to back out.

Add a screenshot or sample output for anything visible.

## Style

- State facts. "Reduces the query from 3 seconds to 200 ms on the test data", not "greatly improves performance".
- No history of your attempts. Only where it ended up and why.
- Mention breaking changes at the top, in words nobody can miss.
- Short. A reviewer reads the first screen.

Do not add credit lines, tool names or signatures unless the project asks for them. Show the user the text before you commit or open the pull request, unless they told you to go ahead.
