---
name: home-food-deli
description: >-
  Order the user's weekly Home Food Deli lunch through the `hfd` CLI, learning their taste over
  time and always confirming before it spends. Use this whenever the user mentions lunch, their
  food/menu order, "what should I get this week", Home Food Deli / HFD, ordering food for a
  colleague or visitor, or asks to see this week's menu or check/cancel an order — even if they
  don't name the CLI. This is the right skill for anything about placing, choosing, reviewing, or
  cancelling their HFD lunch, and for teaching it what they like or dislike.
---

# Home Food Deli — weekly lunch ordering

Order lunch for the user through the `hfd` binary, pick well by learning their taste, and **never
place or cancel an order without an explicit yes.** You are the taste + conversation layer; `hfd`
is the safe hands that talk to the backend, and `scripts/hfd-plan.py` does the week math and scoring.

## The one rule
Ordering spends real money on real food. **Propose, then wait for a clear "yes" before running
`hfd order`.** No yes → no order. If the user is ambiguous ("looks good"?), confirm the exact item
+ date once before spending.

## Setup you can assume
- `hfd` is on PATH (or ask the user for its path). It self-describes: `hfd help --json`.
- The personal token lives in `HFD_TOKEN` or `~/.config/hfd/token`. If `hfd order`/`orders` returns
  an `AUTH` error, tell the user to set it — don't try to work around it.
- Learned taste lives in `~/.config/hfd/preferences.json` (schema: `references/preferences-schema.md`).
  It may not exist yet — that's a cold start, handle it (below).

## Weekly flow
1. **Plan.** Run the planner — it finds the one orderable menu for next week and ranks its items:
   ```
   python3 scripts/hfd-plan.py --hfd "$(command -v hfd)"
   ```
   It returns `{delivery_week, menu_id, cold_start, days:[{date, weekday, ranked:[{name,score,reasons}], excluded}]}`.
   Trust its `menu_id` and dates — it replicates the backend's week rules exactly. Don't compute
   dates yourself; the backend rejects wrong-week items and the math is easy to get subtly wrong.
2. **Propose.** For the day the user wants (default: ask which day, or all five if they want a full
   week), show the top 1–3 ranked items with the *reasons* the planner gave, and the delivery date.
   Keep it short — a human picking lunch wants "Here's my pick: X for Mon 24th. Good?", not a wall.
3. **Confirm.** Wait for a yes. If they pick a different item or say "not that", that's a learning
   signal — capture it in step 5.
4. **Order.** Only now — and pass the user's standing note from prefs `default_note` (verified live
   to save as the order's `special_requirements`, e.g. "spicy very spicy"):
   ```
   hfd order <menu_item_id> <YYYY-MM-DD> --note "<default_note>"
   ```
   Report the result plainly. If it returns `ORDERING_CLOSED`, the cutoff passed — tell them, don't
   retry. If `UNKNOWN_OUTCOME`, do NOT re-order; run `hfd orders` and tell them what you see.
5. **Learn.** Update `~/.config/hfd/preferences.json` from what just happened (see below).

## Cold start (`cold_start: true`, or empty prefs)
You know nothing yet, so don't pretend to. Show a small spread across the categories for that day
(a chicken one, a veg one, a light one, etc.), say you're still learning their taste, and ask what
appeals. Whatever they pick or veto becomes the first entries in the prefs file. A few weeks of this
and the ranking carries itself.

## Learning — how to update preferences.json
The planner reads weights straight from this file, so **a taste change is a data edit, not a code
change.** After each interaction, reflect what you genuinely learned — and mark uncertainty instead
of inventing certainty:
- **They confirmed a pick** → ensure a `likes` pattern covers it (e.g. a `"chicken"` or
  `"chicken curry"` pattern), nudge its `weight` up a little, and append a `history` row
  `{date, order_id, item, result:"ordered"}`.
- **They rejected/vetoed something** → add or strengthen a `never` entry (with the reason they gave)
  if it's a standing dislike, or log a one-off in `overrides` with a `verdict` if it was situational.
  Don't promote a one-time "not today" to a permanent NEVER — ask if you're unsure.
- **They said something general** ("spicy everything", "no seafood ever") → encode it as a broad
  `likes`/`never` pattern and date it in `overrides` so the reasoning is traceable later.
Keep patterns simple substrings (matched case-insensitively against item name + category). After
writing the file, read it back to confirm the change landed before telling the user it's saved.

There may already be historical taste data in the user's `whydev-claw` repo
(`memory/food-preferences.md`) — if the user wants a warm start, offer to seed `preferences.json`
from it; otherwise start fresh and self-learn.

## Other things they may ask
- **"What's on this week / next week?"** → the planner (or `hfd menu --menu-id <id>`), read-only.
- **"What did I order?"** → `hfd orders`.
- **"Cancel it"** → confirm which order, then `hfd cancel <order_id>`.
- **Order for a guest/colleague** → guest ordering (`--for`) is currently **disabled** in the CLI
  (unverified backend behaviour). Tell the user it's not wired up yet rather than faking it.

## Full CLI surface
Run `hfd help --json` for the authoritative command + error-code list. Everything the credentials
can do is reachable; `hfd call` / `hfd get` are raw passthroughs if you ever need something the named
commands don't cover.
