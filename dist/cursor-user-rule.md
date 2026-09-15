# Cursor User Rule (paste into Settings → Rules → User Rules)

> Cursor's user-global rules are server-synced and configured via the UI —
> there is no on-disk file path that registers a global rule. Open Cursor →
> **Settings → Rules → User Rules** and paste the body below. The leading
> `#` heading is optional; everything after the rule line is the rule body.

```
You have access to a local cross-tool memory store via the `recall`
MCP server (shared with Claude Code, Codex, and Hermes). Use it proactively — do not
wait to be asked.

At the start of any substantive task (writing code, refactoring, debugging,
recommending architecture), call `recall_summary` with the absolute project
path and budget ~600 BEFORE doing the work. Read the returned block:
- Pins (📌 #N) — top priority. Respect them.
- Instructions — runbooks the user captured. Follow them.
- Decisions / preferences / feedback — durable choices and corrections.
  Treat as binding unless the user contradicts them in the current message.
- Recent topics — user-turn excerpts from prior sessions. Often contains
  infrastructure facts and paths not yet distilled into decisions.
- Recent sessions / files — context for what was worked on lately.

Skip recall_summary only for trivial tasks (typos, formatting).

When the user asks "what is X" / "where is X" / "do you know X" / "what's
my X config" — call `recall_search` FIRST with `project='all'`. Read
turn-level hits, not just decisions. Most infra facts live in past
conversation turns that were never recorded as formal decisions. Only
fall back to `recall_decisions` / `recall_summary` if search returns
nothing. Never answer "I don't remember" / "not in memory" without
running `recall_search` first.

Mid-task, when you need prior context, call `recall_search` with mode=
'hybrid' and a natural-language query. It fuses lexical + semantic across
every wired tool's history.

Persist durable items without being asked:
- `record_decision(text, kind, project)` — kind='instruction' for runbooks,
  'feedback' for corrections, 'preference' for soft choices, 'fact' (default)
  for stable knowledge. Do NOT call this for one-off task instructions in the
  current conversation.
- `pin_for_session(text, project)` — for things to keep top-of-mind for the
  rest of the session ("goal: ship X today", "the constraint is W").

When referring to a stored item, cite its id (e.g. "per #15, this is macOS-
only"). Stable IDs let the user verify or remove a memory.

Privacy: wrap sensitive content in `<private>...</private>` before quoting
back. Content inside those tags is stripped from the index.

Don't ask, just use it. If you'd normally say "I don't have context on..."
or "remind me what we decided about...", call `recall_search` first.
```

After pasting, restart Cursor once. Verify by asking any non-trivial coding
task without mentioning memory — Cursor should call `recall_summary` first.

## Workspace-scoped alternative

If you'd rather have rules live as files in version control, drop a copy at
`<repo>/.cursor/rules/recall.mdc` with this frontmatter:

```mdc
---
description: Use the recall MCP server proactively for cross-tool memory.
alwaysApply: true
---
<rule body — same as the User Rules content above>
```

Workspace rules apply only to that repo. The User Rules tier is the only way
to get truly global behaviour.
