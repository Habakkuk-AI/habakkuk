---
name: code-review
description: Review a diff or pull request for bugs and risks, most serious first. Use when asked to review code.
---

# Code review

Find what would hurt in production. Style comes last, if at all.

## Understand the change

1. Read the description, the linked issue and the commit messages. Know what the change is meant to do.
2. Read the whole diff once, quickly, for its shape.
3. Then read it again slowly, with the surrounding code open. A diff hides what it did not touch.

If the change is large, say so and suggest splitting it. Review what is there anyway.

## Look for, in this order

1. **Correctness**: does it do what it claims? Edge cases: empty input, zero, one item, very large input, missing values, duplicates, text in another language.
2. **Failure**: what happens when the network, the disk, the database or another service fails halfway? Is anything left half-done?
3. **Security**: input from outside used without checking, secrets in code or logs, missing permission checks, queries or commands built by joining strings.
4. **Data**: migrations that cannot be undone, changes that break existing records, anything that deletes.
5. **Concurrency**: shared state, ordering, things that can run twice.
6. **Compatibility**: public interfaces, config, stored formats and callers you can find by searching.
7. **Tests**: do they test the behaviour that changed, and would they fail if the change were wrong?
8. **Clarity**: names that mislead, a function doing three jobs, a comment that no longer matches.

Run the tests and the linter if you can, and say what you ran.

## Write the review

- Start with one or two sentences: what the change does and your overall view.
- Then findings, most serious first. For each: the file and line, what is wrong, why it matters, and a concrete fix.
- Label each: **must fix**, **should fix**, or **optional**.
- Ask a question when you are not sure. "What happens if the list is empty here?" is a fair comment.
- Say what is good when it is worth repeating elsewhere.

## Manner

- Comment on the code, not the author.
- Verify before you assert. Trace the path or run it. A wrong review comment costs more than a missed one.
- Do not ask for changes that are only your taste. Follow the project's existing conventions.
- No pile of small remarks. If there are ten of one kind, say it once.

Give the review to the user. Post it on the pull request only if they ask.
