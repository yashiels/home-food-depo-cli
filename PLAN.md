# Home Food Deli CLI + Skill — End-to-End Plan (FINAL, for sign-off)

## 0. Goal & shape
A single static **Go** binary `hfd` that an autonomous AI agent shells out to, to order the
user's lunch from Home Food Deli (HFD). A later **Claude/agent skill** wraps the CLI and holds
all purchasing *policy*. User directive: the CLI must **expose everything the credentials can do**
(full agent access); it must not artificially restrict capability.

Core architecture decision (from Oracle R2): **"full capability" and "safe purchasing" cannot
both be enforced by the CLI.** So:
- **CLI** = full capability + truthful outcomes + transport safety. No purchasing policy.
- **Skill (phase 2)** = authority: budget, cadence (1 order/week), human-in-loop, dedupe intent,
  prompt-injection resistance.

Repo: `/Users/yashielsookdeo/Developer/yashiels/home-food-depo-cli`
(GitHub remote: github.com/yashiels/home-food-depo-cli).

## 1. Complete backend surface (fully reverse-engineered — a Supabase app)
Base: `https://isqknoojwebcomqrirog.supabase.co`

### A. Edge Functions — auth = user's personal token `hfd_...` (Bearer). Exactly 3 exist (brute-forced ~35 names):
- `POST /functions/v1/place-order` body `{menu_item_id, delivery_date:"YYYY-MM-DD", ...}`.
  Confirmed: missing menu_item_id → 400; missing delivery_date → 400; past date → 422.
  UNVERIFIED: guest field (web uses `order_name` with user_id:null); Friday-cutoff enforcement.
- `GET  /functions/v1/my-orders` → `{"orders":[...]}`.
- `POST /functions/v1/cancel-order` body `{order_id}`.

### B. REST catalog — anon key (public JWT from the web bundle), RLS-gated:
- Readable: `menus`, `menu_items`, `companies` (+ `order_details`, `company_invites` — see §7 leak).
- Blocked to our token ([] under anon RLS): orders, profiles, guest_recipients, settings,
  user_api_tokens, sheets_sync_failures.
- menu_items: id(uuid), menu_id, day_of_week(Mon..Fri), category, name, sort_order.
  menus: id, year, quarter, quarter_week, published_at. (NO date field — cannot compute delivery dates.)

### C. Ordering window (business rule): order Fri → deliver next Mon; current week is closed.
Enforcement is server-side (settings.cutoff_times + companies.order_days + 09:00 cutoff), all behind
RLS the token cannot read. CLI must NOT recompute it — pass the date, surface the server's verdict.

## 2. CLI command surface (9 commands; JSON is the DEFAULT output)
- `menu [YYYY-MM-DD] [--menu-id <id>]` — items for a menu. Default = newest published_at, returned
  as a LABELED heuristic (not authoritative). Date filter is WEEKDAY-ONLY (documented).
- `menus` — list published menus: id, year, quarter, quarter_week, published_at.
- `order <menu_item_id> <YYYY-MM-DD>` — self-order. `--for "<name>"` DISABLED until V1 verifies it.
- `orders` — my-orders.
- `cancel <order_id>` — cancel (with reconciliation, §5).
- `call --method GET|POST <function> [json|-]` — generic edge-function passthrough (body arg or
  stdin `-`). Token sent ONLY here + named write cmds. The real full-access escape hatch.
- `get <table> [querystring]` — generic PostgREST read via anon key.
- `next` — calendar hints ONLY, tagged `authoritative:false`; never labeled "orderable".
- `help` / `help --json` / `<cmd> --help` — self-describing (§6).

## 3. Agent-caller contract (JSON envelope)
- Success: `{"version":1,"ok":true,"data":...}`. Error: `{"version":1,"ok":false,"error":{"code","message","retryable"}}`.
  Exactly ONE JSON document per invocation, on success AND error. Non-zero exit on any failure
  (never exit 0 with ok:false).
- Error codes: USAGE, VALIDATION, AUTH, REMOTE, ORDERING_CLOSED, UNKNOWN_OUTCOME.
  Assign ORDERING_CLOSED only on a stable observed backend response; else REMOTE + status/details.
- Named-command validation = STRUCTURAL only (valid UUID, parseable date, non-empty guest). `call`/
  `get` bypass validation (full-access hatch).

## 4. Transport safety (hard requirements)
Fixed Supabase origin; escaped path segments; NO arbitrary URLs / base-url flags / redirects. Cap
response-body size. Preserve HTTP status + useful headers (Content-Range). Short total HTTP timeout.
Never put the token in argv (use stdin `-`), logs, output, or errors. Refuse a token file with loose
perms. Fixed UTC+2 SAST offset (NOT time.LoadLocation — fails in minimal static builds); injectable
clock for tests. Treat ALL backend strings as UNTRUSTED (prompt-injection surface).

## 5. Ambiguous-write reconciliation (place-order & cancel-order)
On ANY ambiguity (timeout, conn reset, 5xx, malformed-success — not just timeout):
1. Snapshot my-orders + record start time BEFORE the POST. 2. NO auto-retry, NO auto-cancel.
3. Poll my-orders briefly; diff by order ID; match on item + exact date + self/guest identity
   (+ created_at if present). 4. Report reconciled success ONLY if exactly one new match; else
   UNKNOWN_OUTCOME retryable:false. Verify my-orders INCLUDES guest orders (else guest reconcile
   impossible). Document: concurrent order invocations cannot be safely deduped without server idempotency.

## 6. Self-describing help
`help` (human) and `help --json` (machine catalog: commands+args+flags, error_codes, envelope_schema,
base_url, function list). `<cmd> --help` per command. No-args → prints help (exit 0). Help text is the
single source of truth; generate README from it.

## 7. Go implementation structure
One package, three files: `main.go` (wiring, exactly-one-JSON emission, exit status), `client.go`
(HTTP/auth/timeouts/clock injection, size caps, no-redirect), `commands.go` (arg parsing + behavior).
No os.Exit / printing inside client/command funcs — return errors to main. Go `flag` stops at first
positional → flags before positionals or a tiny hand-rolled parser. Stdlib only; no third-party deps.
`go.mod` (go 1.22+). Anon key embedded (public). Token from `HFD_TOKEN` or `~/.config/hfd/token` (0600).

## 8. Tests (table-driven, stubbed HTTP client, no network)
Every rejected/invalid input performs ZERO POSTs. Cases: UUID/date validation; weekday filter;
JSON envelope shape (success+error); ambiguous-write → UNKNOWN_OUTCOME with exactly-one-match logic;
help --json is valid JSON. Injected clock for date tests.

## 9. Build-time live verification (needs 1 real order + immediate cancel, WITH the user)
- V1: does place-order accept a guest field (`order_name`?) and does my-orders return guest orders?
  → gates enabling `--for`.
- V2: does place-order enforce the Friday/next-Monday cutoff? Test an out-of-window date, not just
  happy path. If NOT enforced → cutoff is a SKILL responsibility + a blocker for unattended ordering.

## 10. Phase 2 — the agent skill (separate deliverable, after CLI is proven)
A skill that: reads `help --json` to learn the CLI; learns the user's food preferences over time
(stored as skill memory, NOT in the binary); picks an item from the current menu; enforces POLICY
(weekly budget, 1 order/week, confirm-with-human before spend if configured, don't double-order by
checking `orders` first); calls `hfd order`. Out of scope for the CLI build; specified here so the
CLI's help/JSON contract is designed to serve it.

## 11. Build plan with subagents (parallelizable)
Orchestrator (me) creates the repo + skeleton (go.mod, main.go stub, dirs), then fans out:
- Agent A — `client.go`: HTTP layer, auth, timeouts, no-redirect, size cap, clock injection, JSON envelope helpers.
- Agent B — `commands.go`: all 9 commands incl. help/help --json, structural validation, reconciliation calls.
- Agent C — tests + README-from-help + `go vet`/`gofmt`/build green.
Serialize where needed: A's client interface is the contract B & C depend on, so define that interface
in the skeleton first. Integration + live V1/V2 done by orchestrator after agents converge.
Deliverables: compiling `hfd` binary, passing tests, README, initial git commit + push.

## 12. Milestones
M1 plan sign-off (this doc) → M2 repo created + skeleton + client interface → M3 subagents build
A/B/C → M4 build+tests green, README generated → M5 live V1/V2 with user → M6 commit+push → (M7 skill).

## Questions for the reviewer (final gate)
1. Is this end-to-end plan complete and internally consistent enough to BUILD now, or are there gaps?
2. Is the subagent split (client / commands / tests) sound given the client-interface dependency, or
   is a different cut safer?
3. Anything in the CLI/skill boundary that will bite the phase-2 skill if we lock the JSON contract now?
4. Any remaining correctness/safety trap not yet covered? Give a clear SIGN OFF or CHANGES REQUIRED.

---
## R3 BLOCKER RESOLUTIONS (authoritative; supersedes conflicts above)

### B1 — double-order race
- Advisory OS file lock (flock on `~/.config/hfd/lock`) held across the whole preflight→POST→
  reconcile critical section for BOTH order and cancel. A second concurrent invocation fails fast:
  `{ok:false,error:{code:"LOCKED"}}`, ZERO POST.
- NEVER auto-replay an in-flight or UNKNOWN_OUTCOME intent.
- SCOPING (ponytail): a durable multi-step intent *journal* is OUT OF CLI SCOPE. This is a personal,
  single-user, ~1-order/week tool; the flock bounds the only realistic race (same machine). Cross-
  machine concurrency is explicitly UNSUPPORTED without server idempotency (documented), and true
  spend-journalling belongs to the phase-2 skill's policy layer. Ceiling named, upgrade path = skill.

### B2 — cancel state machine (SEPARATE from order; do not reuse "exactly one new match")
- Preflight: fetch my-orders; if snapshot fetch fails → ABORT, zero POST. Confirm target order_id is
  present AND active. If absent/already-canceled → return ok with data.final_state accordingly, NO POST.
- POST cancel exactly once.
- On ambiguity: re-fetch my-orders; success ONLY if target ID is now absent or status=canceled; else
  UNKNOWN_OUTCOME retryable:false. Never auto-retry.
- V1 must establish my-orders semantics: does it retain canceled orders? is it paginated? is it
  authoritative? (prototype only proved reachability.)

### B3 — menu↔delivery-date correctness (V2 expanded)
V2 live test must record whether the server REJECTS: (a) weekday mismatch (item day ≠ date weekday),
(b) stale/wrong-menu item for a delivery week, (c) out-of-window/cutoff date. Until the server is
proven to enforce item↔date correctness, the CLI does NOT assert an item is "orderable" for a date;
`menu` output carries `date_binding:"weekday-only, not authoritative"` + menu_id + published_at, and
unattended ordering stays SKILL-GATED (human confirm). No client-side date derivation.

### B4 — versioned JSON contract (locked)
- Envelope: `{version:1, ok:bool, data?|error?}`. version bumps on breaking change; skill pins it.
- One-JSON rule reworded: MACHINE commands emit exactly one JSON doc. `help`/`<cmd> --help` are plain
  TEXT (human); `help --json` is the machine catalog. Non-JSON output only for explicit human help.
- error = `{code, message, retryable, status?, details?}`; status+details REQUIRED for REMOTE.
- Per-command data schemas (v1, stable — the skill depends on these):
  - order  → `{order_id, status, menu_item_id, delivery_date, order_name|null, self:bool}`
  - orders → `{orders:[{order_id, status, menu_item_id, item_name?, delivery_date, order_name|null, created_at?}]}`
  - cancel → `{order_id, final_state:"canceled"|"absent"|"unknown"}`
  - menu   → `{menu_id, published_at, date_binding, items:[{id, day_of_week, category, name, sort_order}]}`
  - menus  → `{menus:[{id, year, quarter, quarter_week, published_at}]}`
  - next   → `{authoritative:false, dates:[{date, weekday}]}`
  - call/get → `{passthrough:true, http_status, body}` (opaque backend data, explicitly not schema-stable)
  - UNKNOWN_OUTCOME error.details → `{operation, request_fingerprint:{menu_item_id,delivery_date,order_name},
    observations:{orders_before, orders_after, new_match_ids:[...]}}` so callers can't mistake/replay it.

### B5 — auth routing (explicit, fixes contradiction)
- Personal `hfd_` token → ALL edge-function calls: place-order, cancel-order, my-orders (incl.
  reconciliation reads) AND `call`. Anon key → PostgREST reads ONLY: `menu`, `menus`, `get`.
- Token NEVER attached to anon REST requests. (Corrects "token only on write cmds" — `orders` reads
  need the token.)

### B6 — explicit acceptance tests (each a named test)
snapshot-failure→zero POST; POST-attempted-exactly-once (stub counter); cancel reconciliation distinct
from order; concurrent-intent lock (2nd invocation → LOCKED, zero POST); redirect refusal; body-size
cap enforced; credential redaction (token never in output/errors/argv — assert stdin `-` path);
auth routing (anon calls carry no token, edge calls carry token); help --json is valid JSON;
UNKNOWN_OUTCOME carries operation+fingerprint+observations.

### Skeleton contract (built BEFORE subagent fan-out; from Oracle Q2)
Define in the skeleton: shared request/result schema TYPES + BOTH reconciliation state machines
(order, cancel) as documented interfaces, so Agents A/B/C share one contract.

---
## R4 PATCHES (final contract fixes; supersede B2/B4 where they conflict)
1. UNKNOWN_OUTCOME.details is OPERATION-DISCRIMINATED:
   - order  → `{operation:"place-order", request_fingerprint:{menu_item_id,delivery_date,order_name}, observations:{orders_before,orders_after,new_match_ids:[...]}}`
   - cancel → `{operation:"cancel-order", order_id, observations:{present_before:bool, present_after:bool|null}}`
2. `cancel` data.final_state ∈ {"canceled","absent"} ONLY. An unresolved cancel is NOT a data state —
   it returns error code UNKNOWN_OUTCOME (retryable:false). ("unknown" removed.)
3. Error-code catalog (complete): USAGE, VALIDATION, AUTH, REMOTE, ORDERING_CLOSED, LOCKED,
   UNKNOWN_OUTCOME.
4. B4 lock is STAGED: `menu`, `menus`, `next`, `cancel`, `call/get`, error + envelope schemas are
   locked now. `order`/`orders` field REQUIREDNESS is provisional — until V1 proves which fields the
   backend actually returns, every order/orders field EXCEPT `order_id` is OPTIONAL. After V1, promote
   the confirmed fields to required and bump nothing (additive). The skill pins `version:1` and treats
   absent optional fields as unknown, never as errors.
