# preferences.json schema (`~/.config/hfd/preferences.json`, chmod 600)

The learned taste store. `scripts/hfd-plan.py` reads it to score menu items; the skill updates it
after each interaction. Weights live here (not in code) so a taste change is a data edit. Keep it
human-readable — the user can open and tweak it.

```json
{
  "version": 1,
  "default_note": "spicy very spicy",
  "likes": [
    {"pattern": "curry", "weight": 18, "note": "always curry - top preference"},
    {"pattern": "lamb",  "weight": 9,  "note": "loves lamb"}
  ],
  "never": [
    {"pattern": "beef",       "reason": "did not enjoy beef curry", "since": "2026-08-17"},
    {"pattern": "cauli-rice", "reason": "won't order the cauli-rice version", "since": "2026-08-17"}
  ],
  "overrides": [
    {"date": "2026-08-17", "action": "curry boosted to always-win", "verdict": "baseline"}
  ],
  "history": [
    {"date": "2026-08-24", "order_id": "…", "item": "Chicken fillet curry", "result": "ordered"}
  ]
}
```

## Fields
- **default_note** — standing order note, sent as `special_requirements` on every `hfd order --note`.
- **likes[]** — `pattern` (case-insensitive substring matched against item name + category),
  `weight` (higher = stronger pull), optional `note` (why — surfaced as the ranking reason).
- **never[]** — `pattern` excludes any matching item outright, with a `reason` and `since` date.
  A hard exclusion, so only put standing dislikes here, not one-offs.
- **overrides[]** — dated log of decisions/changes with a `verdict`. This is the audit trail: when a
  weight changed or a one-off happened, record why so future-you can trace the reasoning.
- **history[]** — what was actually ordered, for pattern-spotting and de-duplication.

## Scoring model (what the planner does)
An item's score = sum of the weights of every `likes` pattern it matches. Any `never` match drops it
to the excluded list regardless of score. Ties and all-zero scores (cold start) mean "no signal yet"
— the skill should present a spread and ask rather than guess.

## Updating rules of thumb
- Confirmed pick → make sure a `likes` pattern covers it, nudge its weight, append a `history` row.
- Standing dislike → add a `never` (with reason). One-off "not today" → an `overrides` entry, not a never.
- Uncertain? Record it as an override with a note and confirm with the user next time — don't
  invent a permanent rule from a single data point.
