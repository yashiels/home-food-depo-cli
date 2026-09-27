# Live V1/V2 findings (2026-08-17, one real order placed + cancelled)

## Proven working end-to-end (real backend)
- place-order → `{order_id, status:"confirmed", menu_item_id, delivery_date, self:true}`. These
  OrderData fields are CONFIRMED returned (no longer provisional).
- my-orders → list rows expose only `{order_id, status, delivery_date}` (NO menu_item_id / item
  name / order_name). Reconciliation must diff by NEW order_id vs snapshot (works), not by item.
- cancel-order → `final_state:"canceled"`; my-orders empty afterward. Cancel state machine verified live.

## V2 — CONFIRMED: server enforces item↔week binding
- Wrong-week item → clean 422 `{"error":"menu_item_id not found in the menu for that week"}` (NON-mutating).
- Delivery 2026-08-24 was valid ONLY with an item from the **quarter_week=1** menu (0e97909f),
  NOT the newest-published menu (quarter_week=2). => "newest published" is the WRONG selector.
- `menus.week_start_date` is null everywhere; no readable date→menu mapping. quarter_week is a
  rotating counter computed client-side (month-based Monday indexing, clamp 4) — fragile to reverse.

## V1 — guest ordering still UNVERIFIED
- Only self-orders tested (a guest order has user_id:null and may not appear in my-orders → could be
  un-cancellable). `--for` stays DISABLED.

## Open design problem (needs decision): menu-week selection
`hfd menu` shows newest-published (wrong week); `hfd order` only works with a valid (item,date) pair.
The orderable-week→menu mapping is not derivable from readable data without reversing the app's
quarter_week(date) formula. Recommend solving this in the SKILL (policy layer + human-in-loop),
keeping the CLI a pass-through, so a formula bug is a skill fix not a CLI rebuild.

# Menu slot binding (2026-09-27)

- This resolves and supersedes the open design problem above.
- The web app maps the SAST Monday of a delivery week onto `menus.slot` using the local anchor
  2026-09-20: `((floor(days_from_anchor / 7) mod 4) + 4) mod 4 + 1`.
- Confirmed examples: 2026-09-14 → slot 4, 2026-09-21 → slot 1, 2026-09-28 → slot 2, and
  2026-10-19 → slot 1.
- Published menus are selected by that slot. Rows with `slot:null` are dead legacy duplicates and
  must not participate in week binding.
- Menu `ada07ea3-cbb8-4baa-809d-1e40cbcc55ee` is slot 2 and was accepted for a real order in the
  delivery week of 2026-09-28.
