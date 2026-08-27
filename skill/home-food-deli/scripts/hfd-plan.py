#!/usr/bin/env python3
"""hfd-plan — decide which menu is orderable next week and rank its items by learned taste.

The script does the deterministic work (week math, menu match, scoring) so the model never has
to do date arithmetic or re-derive the ordering rules. The model's job is to take this JSON,
talk to the human, and — only after a yes — call `hfd order` and record the result.

Output (stdout, one JSON doc):
  {"ok":true, "delivery_week":"2026-Www", "menu_id":..., "days":[
     {"date","weekday","ranked":[{"id","name","category","score","reasons":[...]}], "excluded":[...]}]}
Exit: 0 actionable · 3 precondition missing (no token / no menu published) · 1 error.

Scoring weights live in the prefs FILE, not here (a taste change is a data edit, not a code edit).
Cold start = empty prefs = every item scores 0 → the model should present a spread and ask.
"""
import argparse, json, os, re, subprocess, sys
from datetime import date, timedelta

DEFAULT_PREFS = os.path.expanduser("~/.config/hfd/preferences.json")


def iso_week(d):
    return d.isocalendar()[1]


def week_menu_key(d):
    """Replicates the app's To(date): ISO-week → (year, quarter, quarter_week).
    quarter = min(4, ceil(isoweek/13)); quarter_week cycles 1..4. Verified against a live order."""
    t = iso_week(d)
    q = min(4, -(-t // 13))
    w = (t - (q - 1) * 13 - 1) % 4 + 1
    return d.year, q, w


def hfd_json(hfd, *args):
    out = subprocess.run([hfd, *args], capture_output=True, text=True)
    try:
        doc = json.loads(out.stdout)
    except json.JSONDecodeError:
        raise RuntimeError(f"hfd {' '.join(args)} produced no JSON (stderr: {out.stderr.strip()[:200]})")
    return doc


def load_prefs(path):
    if not os.path.exists(path):
        return {"version": 1, "likes": [], "never": [], "overrides": [], "history": []}
    with open(path) as f:
        p = json.load(f)
    for k in ("likes", "never", "overrides", "history"):
        p.setdefault(k, [])
    return p


def score_item(item, prefs):
    """Substring match (case-insensitive) of item name+category against like/never patterns.
    Returns (score, reasons, excluded_reason_or_None)."""
    hay = (item.get("name", "") + " " + item.get("category", "")).lower()
    for n in prefs["never"]:
        pat = n.get("pattern", "").lower()
        if pat and pat in hay:
            return 0, [], f"never-order: '{n['pattern']}'" + (f" ({n['reason']})" if n.get("reason") else "")
    score, reasons = 0, []
    for l in prefs["likes"]:
        pat = l.get("pattern", "").lower()
        if pat and pat in hay:
            w = int(l.get("weight", 1))
            score += w
            reasons.append(f"+{w} '{l['pattern']}'" + (f" ({l['note']})" if l.get("note") else ""))
    return score, reasons, None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--hfd", default="hfd", help="path to the hfd binary")
    ap.add_argument("--prefs", default=DEFAULT_PREFS)
    ap.add_argument("--today", help="override today (YYYY-MM-DD) for testing")
    ap.add_argument("--weekday", help="only plan this weekday (Monday..Friday)")
    ap.add_argument("--week", choices=["this", "next"], default="next",
                    help="which delivery week to plan; 'this' surfaces the current week's remaining days (server still owns the cutoff)")
    args = ap.parse_args()

    today = date.fromisoformat(args.today) if args.today else date.today()
    prefs = load_prefs(args.prefs)

    # Delivery week: 'next' (default) or 'this' (current week's remaining days; server owns the cutoff).
    if args.week == "this":
        next_mon = today - timedelta(days=today.weekday())
    else:
        next_mon = today + timedelta(days=(7 - today.weekday()))
    y, q, w = week_menu_key(next_mon)

    try:
        menus = hfd_json(args.hfd, "menus").get("data", {}).get("menus", [])
    except RuntimeError as e:
        print(json.dumps({"ok": False, "error": str(e)})); return 3
    match = [m for m in menus if m["year"] == y and m["quarter"] == q and m["quarter_week"] == w]
    if not match:
        print(json.dumps({"ok": False, "error": f"no published menu for Q{q} wk{w} {y} (delivery week of {next_mon})",
                          "hint": "menu may not be published yet"})); return 3
    menu_id = match[0]["id"]

    menu = hfd_json(args.hfd, "menu", "--menu-id", menu_id).get("data", {})
    items = menu.get("items", [])

    weekdays = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday"]
    if args.weekday:
        weekdays = [args.weekday.capitalize()]
    days = []
    for i, wd in enumerate(weekdays):
        d = next_mon + timedelta(days=i if not args.weekday else weekdays_index(wd) )
        if args.week == "this" and d < today:
            continue  # ponytail: current-week planning drops days already past
        day_items = [it for it in items if it.get("day_of_week") == wd]
        ranked, excluded = [], []
        for it in day_items:
            s, reasons, excl = score_item(it, prefs)
            row = {"id": it["id"], "name": it.get("name", ""), "category": it.get("category", ""),
                   "score": s, "reasons": reasons}
            (excluded if excl else ranked).append({**row, "excluded": excl} if excl else row)
        ranked.sort(key=lambda r: (-r["score"], r["name"]))
        days.append({"date": d.isoformat(), "weekday": wd, "ranked": ranked, "excluded": excluded})

    cold = not prefs["likes"] and not prefs["never"]
    print(json.dumps({"ok": True, "delivery_week": f"{next_mon.isocalendar()[0]}-W{next_mon.isocalendar()[1]:02d}",
                      "menu_id": menu_id, "cold_start": cold, "days": days}, indent=2))
    return 0


def weekdays_index(wd):
    return ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday"].index(wd.capitalize())


if __name__ == "__main__":
    sys.exit(main())
