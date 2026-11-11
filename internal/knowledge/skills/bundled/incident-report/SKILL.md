---
name: incident-report
description: Write a blameless report after an outage or mistake, with timeline, causes and actions. Use after something went wrong.
---

# Incident report

The point is to stop it happening again. Blame makes people hide things, so describe what happened and why the system allowed it, not who slipped.

## Collect the facts

Gather before writing, and note where each fact came from:

- alerts, logs, dashboards and monitoring graphs,
- the chat or call where it was handled,
- deploys, config changes and commits around that time,
- reports from customers or users.

Put every time in one time zone and say which. Ask the people involved what they saw and did. Do not fill gaps from guesswork. Mark a gap as unknown.

## Write

1. **Summary**: three or four sentences. What broke, for whom, for how long, and that it is fixed.
2. **Impact**: who was affected and how, in numbers where you have them. Failed requests, lost orders, hours down, data affected.
3. **Timeline**: time, then what happened. Start at the first cause, not the first alert. Include when it was noticed, each action taken and when it was resolved.
4. **Causes**: what made it happen and what let it through. Ask "why" until you reach something that can be changed: a missing check, an unclear procedure, a hidden dependency. There is usually more than one.
5. **What went well, and what was slow**: detection, communication, the fix. Be specific.
6. **Actions**: a table of action, owner and date. Each action removes a cause or shortens the next incident. "Be more careful" is not an action.
7. **Open questions**: what is still not understood.

## Tone

- Roles, not names, unless the people agree: "the engineer on call", not a person.
- Plain statements of fact. No "unfortunately", no excuses.
- Say what is known and what is assumed.
- Write so that someone outside the team can follow it. Explain internal terms.

## Care

- Leave out secrets, customer data and details an attacker could use. For a security incident, ask who may read the report before sharing it.
- If customers were affected, offer a short public version: what happened, what it meant for them, what has been done. No internal detail.
- Do not publish or send the report. Hand it to the user.

Offer to add the actions to the board and to check on them in two weeks.
