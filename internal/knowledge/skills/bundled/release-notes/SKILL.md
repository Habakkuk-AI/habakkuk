---
name: release-notes
description: Write release notes or a changelog entry from merged changes, in users' words. Use when preparing a release.
---

# Release notes

Release notes are for the people who use the software. They want to know what is different for them and whether they must do anything.

## Gather

1. Find the range: from the last release tag to now. `git log LAST..HEAD` and the merged pull requests give the list.
2. Read each change, not only its title. Titles are written for developers and often miss the effect.
3. Check the project's existing changelog for its format, headings and tone, and follow them.

## Sort

Group by what the reader cares about:

- **Breaking changes**: anything that needs action before or after updating. Always first.
- **Added**: new things users can do.
- **Changed**: behaviour that is different.
- **Fixed**: bugs users may have hit.
- **Security**: fixes with a security effect. Follow the project's disclosure rules on how much to say.
- **Removed or deprecated**: what is gone or going, and what to use in its place.

Leave out what users cannot notice: refactoring, test changes, internal renames, dependency bumps with no effect. Put them in one line at the end, or nowhere.

## Write each entry

- Start with what the user can now do, or what is now different. "You can export a report as PDF", not "Add PDF exporter class".
- One or two sentences. Name the command, setting or screen involved.
- For a breaking change: what breaks, who is affected, and the exact steps to take.
- For a fix: describe the symptom people saw, so they recognise it.
- Link the issue or pull request for those who want detail.
- Credit outside contributors the way the project usually does.

## Top of the notes

Two or three sentences on what this release is about, then any required action in plain view. Give the version and the date.

## Check

- Every breaking change is listed, with steps.
- Nothing is claimed that the code does not do. Try the headline features if you can.
- Version numbers, commands and links are right.
- No internal names, ticket codes or private details that mean nothing to a user.

Show the draft to the user. Publish, tag or post it only when they say so.
