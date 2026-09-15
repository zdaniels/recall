# recall (always-on)

You have access to a local cross-tool memory store via the `recall` MCP server. The store is shared with Codex, Cursor, and Hermes on this machine. Use it proactively — do not wait to be asked.

## At the start of every substantive task

The SessionStart hook already injects an `<recall>` block. It opens with a **Ground truth** line — the items beneath it are established for this project; treat them as authoritative, prefer them over re-deriving or re-asking, and if you must contradict one, say so and call `record_decision`. Read it carefully:

- **Pins** (`📌 #N`) — top priority. The user pinned these for a reason; respect them.
- **Instructions** — runbooks the user has captured (e.g. release procedures). Follow them.
- **Decisions / preferences / feedback** — durable choices and corrections. Treat as binding unless the user contradicts them now.
- **Recent topics** — user-turn excerpts from prior sessions in this project. Often contains infrastructure facts, paths, and procedures that haven't been distilled into formal decisions yet.
- **Recent sessions / files** — context for what was worked on lately.

## When the user asks "what is X" / "where is X" / "do you know X"

This includes things like *"what's my github runner config?"*, *"where does the auth manifest live?"*, *"which AWS profile is dev?"*, *"do you remember the API base URL?"*. The data is almost always in past conversation turns even when it's not in formal decisions.

**Procedure (mandatory, in order):**

1. Call `recall_search` with the natural-language question and `mode='hybrid'`. Do NOT filter by project unless you have a specific reason — pass `project='all'` so cross-project facts surface.
2. Read every turn-level hit, not just decision-level hits. Most infrastructure facts (cluster names, manifest paths, environment-variable names, IAM patterns) live in turn excerpts that were never explicitly recorded as decisions.
3. **Only after** `recall_search` returns nothing relevant, fall back to `recall_decisions` / `recall_summary`.
4. **Never** answer "I don't remember" / "not in memory" / "I don't have context on..." without first running `recall_search`. The store is searchable; assume the answer is there until proven otherwise.

When you do find the answer in a past turn, cite the session id + date so the user can verify (e.g. *"per session 56abb5da on 2026-05-09: …"*).

## Mid-task recall

When you need to remember a prior decision, conversation, or file context, call `recall_search` with `mode='hybrid'` (default) and a natural-language query. This fuses lexical FTS5 + semantic cosine search across both turns and decisions, across every tool wired to recall (Claude Code, Codex, Cursor, Hermes).

## Capturing durable items

When the user says something durable, persist it without being asked:

- `record_decision(text, kind='fact'|'preference'|'feedback'|'instruction', project=<abs path>)` — for stable rules, preferences, corrections, and runbook procedures. Use `kind='instruction'` for runbook content ("to deploy X, run Y then Z"). Use `kind='feedback'` for corrections ("don't push to main without review"). Otherwise default to `kind='fact'`.
- `pin_for_session(text, project=<abs path>)` — for things to keep top-of-mind for the rest of the session ("goal: ship migration today", "remember the constraint is W"). Pins surface first in every recall block.

Be aggressive about infrastructure facts. *Cluster names, manifest paths, AWS profile patterns, IAM ARNs, credential conventions, deploy procedures, file-layout conventions* — all of these are durable and worth a `record_decision` call even if the user didn't say "remember this." Don't wait for a runbook procedure to feel important; capture it the first time you discover it.

Do NOT call `record_decision` for one-off task instructions inside the current conversation. Reserve it for things the user clearly wants future-you (or future-Codex / future-Cursor) to remember.

## Decision IDs

When referring to a stored decision, use its id (e.g. *"per decision #15, this project uses pnpm not npm"*). Stable IDs make it easy for the user to verify or remove a memory.

## Privacy

Wrap sensitive content in `<private>...</private>` before quoting it back. Content inside those tags is stripped before being indexed or surfaced to other tools.

## Don't ask, just use it

The user has paid for this substrate; they expect it to be consulted. If you would normally say *"I don't have context on..."* or *"could you remind me what we decided about..."*, instead call `recall_search` first.
