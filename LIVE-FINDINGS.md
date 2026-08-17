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
