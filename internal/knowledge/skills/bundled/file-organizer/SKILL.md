---
name: file-organizer
description: Tidy a folder by proposing a structure and names, then moving files once the user agrees. Use to organise files.
---

# File organizer

Moving files is easy to get wrong and tedious to undo. Plan first, show the plan, then move.

## Look

1. List the folder the user named. Count the files, and note their types, sizes and dates.
2. Work only inside that folder. Do not touch system folders, application folders, hidden files or anything belonging to a program.
3. Spot the patterns: duplicates, downloads never opened, screenshots, several versions of one document, very large files.

## Propose

Show a plan before changing anything:

- The folder structure, at most two levels deep. Group by what the user will look for: project, year, or kind of document. Choose one scheme, not a mix.
- The naming pattern, for example `2026-03-14 invoice plumber.pdf`: date first so files sort, lowercase, hyphens for spaces.
- A table of what moves where. For a large folder, show the rules and ten examples.
- What you would leave alone, and why.
- Likely duplicates and large files, listed for the user to decide. Never delete on your own.

Wait for a yes. Apply changes the user asks for to the plan and show it again if it changed a lot.

## Move

- Write a log first: every old path and its new path, in a file in the folder. That log is the undo.
- Move, do not copy, so nothing is left behind twice.
- Never overwrite. If a name is taken, add a number.
- Keep dates and extensions as they are.
- Stop at the first error and report it.

## Report

Say how many files moved, how many were left, where the log is, and how to put everything back. Offer to keep the folder tidy on a schedule only if the user asks.
