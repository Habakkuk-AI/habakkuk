---
name: write-a-skill
description: Write or improve a skill, a SKILL.md an agent can follow. Use to teach a repeatable procedure.
---

# Writing a skill

A skill is a page of instructions for a job that comes up again. Write one when you have done something twice and would want to do it the same way a third time.

## The file

A folder named after the skill, with a `SKILL.md` inside:

```markdown
---
name: weekly-report
description: Write the weekly report from the board and the week's sessions. Use at the end of the week or when asked for the report.
---

1. ...
```

- **name**: lowercase letters, digits and hyphens, the same as the folder. It says what the skill does: `invoice-chaser`, not `helper-2`.
- **description**: one line with two parts, what the skill does and when to use it. This line is all an agent sees when deciding whether to load the skill, so put the words a user would say in it.
- **requires** (optional): what the skill needs from the machine. `requires: {bins: [gh], env: [GITHUB_TOKEN]}` hides the skill where the program or the variable is missing.

Save your own skills with `skill_save`. To change a skill that came with Habakkuk or was installed, save a copy under the same name. Yours takes its place.

## The instructions

Write the way you would brief a capable colleague on their first day:

1. **What to find out first**: the inputs the job needs, and what to ask when one is missing.
2. **The steps**, in order, each starting with a verb. Exact command names, tool names and options where they matter.
3. **The rules**: what must always or never happen, and why in a few words.
4. **How to check the result** before saying it is done.
5. **When to stop and ask the user**: anything that sends, pays, deletes or cannot be undone.

Keep it to what is specific to this job. Leave out general advice an agent already follows.

## Make it good

- Under about 300 lines. Move long reference material into supporting files beside `SKILL.md` and say when to read them.
- Concrete over abstract: a real example of good output beats a paragraph describing it.
- Say what the result looks like: its sections, its length, its format.
- Name the pitfalls you actually hit.
- Use invented names and numbers in examples. No real people, passwords, keys or customer details.
- Prefer instructions to scripts. A skill with a script stays switched off on other people's machines until they approve it.

## Check it

Try the skill on a real case and read the result as a stranger would. Fix whatever you had to explain on top. If a skill of yours already covers this, improve that one. Two skills for one job means the wrong one gets loaded.

Tell the user the skill's name, what it covers and that they can run it with `/` followed by its name.
