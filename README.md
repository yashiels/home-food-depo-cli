# hfd — Home Food Deli CLI

A single static Go binary that an autonomous AI agent (or a human) shells out to in order to read
the Home Food Deli menu and place, list, or cancel lunch orders.

JSON is the default output. Every machine command emits **exactly one JSON document** on stdout, on
success and on failure alike. The CLI exposes the full capability of your credentials and holds **no
purchasing policy** — budget, cadence and human-in-the-loop confirmation belong to the calling agent
or skill.

## Build

```sh
go build -o hfd .
```

Go 1.22+, standard library only, no third-party dependencies.

## Token setup

The personal `hfd_` token authenticates every edge-function call: `order`, `orders`, `cancel` and
`call`. PostgREST reads (`menu`, `menus`, `get`) use the public anon key and never carry the token.
`next` needs no credentials at all.

```sh
export HFD_TOKEN=hfd_xxx
```

or write it to a file:

```sh
mkdir -p ~/.config/hfd
printf 'hfd_xxx' > ~/.config/hfd/token
chmod 600 ~/.config/hfd/token
```

A token file with permissions looser than `0600` is refused. The token is never printed, logged, or
placed in argv — use the `call ... -` stdin form when a body needs to carry one.

## Commands

| Command | Description |
|---|---|
| `menu [YYYY-MM-DD] [--menu-id <id>]` | Items for a menu; default is the newest published menu. A date filters by **weekday only**. |
| `menus` | List published menus, newest `published_at` first. |
| `order <menu_item_id> <YYYY-MM-DD>` | Place a self-order. |
| `orders` | List my orders. |
| `cancel <order_id>` | Cancel an order (preflight + reconciliation). |
| `call --method GET\|POST <function> [json\|-]` | Generic edge-function passthrough; bypasses all validation. |
| `get <table> [querystring]` | Generic PostgREST read with the anon key; bypasses all validation. |
| `next` | Next week's Monday–Friday dates in SAST. Hints only. |
| `help [--json]` | Human help text, or the machine-readable catalog. |

`hfd <command> --help` prints per-command help. `hfd` with no arguments prints the help text and
exits 0. `help` output is plain text, not the JSON envelope; `help --json` is the machine catalog.

### Examples

```sh
hfd menus
hfd menu                       # newest published menu, all items
hfd menu 2026-03-16            # same menu, Monday items only
hfd menu --menu-id 1212...9090

hfd next                       # candidate delivery dates (hints, not authoritative)
hfd order 1111...5555 2026-03-16
hfd orders
hfd cancel aaaa...eeee

hfd get menus 'select=id,published_at&order=published_at.desc'
hfd call --method GET my-orders
printf '{"order_id":"aaaa...eeee"}' | hfd call --method POST cancel-order -
```

## JSON envelope

Success:

```json
{"version":1,"ok":true,"data":{"...":"command-specific"}}
```

Error:

```json
{"version":1,"ok":false,"error":{"code":"...","message":"...","retryable":false,"status":500,"details":{}}}
```

Exit codes: `0` only on `ok:true`, `64` on `USAGE`, `2` on any other error. The CLI never exits 0
with `ok:false`.

`status` and `details` are always present for `REMOTE`-class errors.

### Error codes

| Code | Meaning |
|---|---|
| `USAGE` | Wrong arguments or an unknown command/flag. Exit 64. |
| `VALIDATION` | Structural check failed (bad UUID, bad date), or the backend rejected the request as invalid. |
| `AUTH` | No usable token, a token file with loose permissions, or the backend rejected the credentials. |
| `REMOTE` | Backend transport failure or an unexpected status/shape. Carries `status` + `details`. |
| `ORDERING_CLOSED` | The backend stated the ordering window is closed for that date. |
| `LOCKED` | Another `hfd order`/`hfd cancel` holds the write lock. Nothing was sent. |
| `UNKNOWN_OUTCOME` | The write was sent but its result could not be confirmed. |

**Never auto-retry `UNKNOWN_OUTCOME`.** Read `hfd orders` and decide. Its `details` are
operation-discriminated:

```json
{"operation":"place-order","request_fingerprint":{"menu_item_id":"...","delivery_date":"..."},
 "observations":{"orders_before":1,"orders_after":1,"new_match_ids":[]}}
```

```json
{"operation":"cancel-order","order_id":"...","observations":{"present_before":true,"present_after":true}}
```

## Write safety

`order` and `cancel` share one protocol:

1. Take an advisory file lock on `~/.config/hfd/lock`. A second concurrent run fails `LOCKED` and
   sends nothing.
2. Snapshot `my-orders`. A failed snapshot aborts with **zero** POSTs.
3. POST **exactly once**. Never auto-retried.
4. On ambiguity (timeout, connection reset, 5xx, unreadable success body) poll `my-orders` and
   reconcile. `order` resolves only on exactly one new matching order; `cancel` resolves only when
   the target is absent or its status is canceled. Anything else is `UNKNOWN_OUTCOME`.

`cancel` returns `final_state` of `"canceled"` or `"absent"` only — an unresolved cancel is an
error, never a data state. An already-canceled or absent order sends no POST at all.

Cross-machine concurrency is unsupported: the lock is local, and the backend offers no idempotency
key.

## Dates

All dates are interpreted in SAST (UTC+2, fixed offset). Menus carry no dates in the backend, so the
CLI never claims an item is orderable for a given date: `menu` output carries
`date_binding: "weekday-only, not authoritative"`, and `next` is tagged `authoritative:false`. The
server owns the cutoff verdict.

`--for` (guest ordering) is currently **disabled** and returns `VALIDATION`.

## Transport

Fixed Supabase origin, path segments escaped, no caller-supplied URLs and no base-URL flag.
Redirects are refused so the bearer token is never replayed to another host. Response bodies are
capped at 2 MiB and the total HTTP timeout is 20s. All backend strings are treated as untrusted:
they are echoed into `details` (truncated), never interpreted.

## For AI agents

Run `hfd help --json` to discover the surface. It returns the contract version, base URL, edge
function list, the full error-code catalog, the envelope schema with exit codes, and every command
with its args, flags, description and notes. Pin `version: 1` and treat absent optional fields as
unknown rather than as errors.

## Tests

```sh
go test ./...
```

The suite is hermetic: no network, no real clock, no real home directory, no sleeps.
