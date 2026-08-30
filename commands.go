package main

// commands.go — argument parsing + behaviour for all 9 commands, plus the
// self-describing help (plan §2, §5, §6, R3 B1/B2, R4 patches).
//
// Rules enforced here:
//   - handlers never print and never call os.Exit; they return data or *CLIError.
//   - named commands do STRUCTURAL validation only (UUID / date shape); `call`
//     and `get` bypass validation entirely (the full-access hatch).
//   - every backend string is untrusted: it is echoed into details, never parsed
//     for meaning beyond the coarse "closed/cut" cutoff classification.
//   - writes (order, cancel) run under an advisory file lock, snapshot my-orders
//     first, POST exactly once, and never auto-retry.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---- small shared helpers -------------------------------------------------

var (
	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// reconcilePolls is the ambiguity poll schedule (var so tests can shorten it).
var reconcilePolls = []time.Duration{300 * time.Millisecond, 700 * time.Millisecond, 1500 * time.Millisecond}

const dateBindingLabel = "weekday-only, not authoritative"

func usageErr(msg string) *CLIError {
	return &CLIError{Code: CodeUsage, Message: msg}
}

func validationErr(msg string) *CLIError {
	return &CLIError{Code: CodeValidation, Message: msg}
}

// validUUID is the only shape check applied to ids on named commands.
func validUUID(s string) bool { return uuidRe.MatchString(s) }

// parseDate accepts a strict YYYY-MM-DD calendar date and returns it in SAST.
func parseDate(s string) (time.Time, bool) {
	if !dateRe.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", s, SAST)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// splitArgs pulls the declared long flags out of args and returns the positionals.
// Supports `--flag value` and `--flag=value`; an undeclared flag is a usage error.
func splitArgs(args []string, want map[string]*string) ([]string, *CLIError) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		name, val := a, ""
		hasVal := false
		if eq := strings.Index(a, "="); eq >= 0 {
			name, val, hasVal = a[:eq], a[eq+1:], true
		}
		p, ok := want[name]
		if !ok {
			return nil, usageErr("unknown flag: " + name)
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, usageErr("flag " + name + " needs a value")
			}
			i++
			val = args[i]
		}
		*p = val
	}
	return pos, nil
}

// truncate bounds any untrusted backend text we echo back to the caller.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// remoteDetails packages an untrusted backend response for the error envelope.
func remoteDetails(status int, body []byte) map[string]interface{} {
	return map[string]interface{}{
		"status":      status,
		"remote_body": truncate(strings.TrimSpace(string(body)), 512),
	}
}

// decodeJSON parses a body into a generic value: parsed JSON, or the raw string
// when it is not JSON at all (used by the passthrough commands).
func decodeJSON(body []byte) interface{} {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return string(body)
	}
	return v
}

// jsonStr reads the first present string-ish key. Numbers/bools are stringified;
// anything else yields "".
func jsonStr(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(t)
		}
	}
	return ""
}

// jsonInt reads the first present numeric key (accepting numeric strings too).
func jsonInt(m map[string]interface{}, keys ...string) int {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case float64:
			return int(t)
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
				return n
			}
		}
	}
	return 0
}

// objects coerces a decoded value into a list of JSON objects.
func objects(v interface{}) []map[string]interface{} {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}

// ---- REST reads (anon key) ------------------------------------------------

// restRows performs a PostgREST read and returns the rows as generic objects.
// Rows are decoded loosely on purpose: a backend schema drift must not turn a
// readable menu into a hard parse failure.
func restRows(d *Deps, table, query string) ([]map[string]interface{}, *CLIError) {
	status, body, err := d.Backend.RestGET(table, query)
	if err != nil {
		return nil, &CLIError{
			Code: CodeRemote, Message: "backend read failed", Retryable: true,
			Status: status, Details: map[string]interface{}{"transport": err.Error()},
		}
	}
	if cerr := classifyRead(status, body); cerr != nil {
		return nil, cerr
	}
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, &CLIError{
			Code: CodeRemote, Message: "backend returned a non-JSON response", Retryable: false,
			Status: status, Details: remoteDetails(status, body),
		}
	}
	rows := objects(v)
	if rows == nil {
		return nil, &CLIError{
			Code: CodeRemote, Message: "backend returned an unexpected response shape", Retryable: false,
			Status: status, Details: remoteDetails(status, body),
		}
	}
	return rows, nil
}

// classifyRead maps a non-2xx read onto the error catalog. nil = success.
func classifyRead(status int, body []byte) *CLIError {
	switch {
	case status >= 200 && status <= 299:
		return nil
	case status == 401 || status == 403:
		return &CLIError{
			Code: CodeAuth, Message: "backend rejected the credentials", Retryable: false,
			Status: status, Details: remoteDetails(status, body),
		}
	default:
		return &CLIError{
			Code: CodeRemote, Message: fmt.Sprintf("backend returned status %d", status),
			Retryable: status >= 500, Status: status, Details: remoteDetails(status, body),
		}
	}
}

// ---- menu / menus ---------------------------------------------------------

const menuSelect = "select=id,year,quarter,quarter_week,published_at"

func menuSummary(m map[string]interface{}) MenuSummary {
	return MenuSummary{
		ID:          jsonStr(m, "id"),
		Year:        jsonInt(m, "year"),
		Quarter:     jsonInt(m, "quarter"),
		QuarterWeek: jsonInt(m, "quarter_week"),
		PublishedAt: jsonStr(m, "published_at"),
	}
}

func cmdMenu(d *Deps, a []string) (interface{}, *CLIError) {
	var menuID string
	pos, cerr := splitArgs(a, map[string]*string{"--menu-id": &menuID})
	if cerr != nil {
		return nil, cerr
	}
	if len(pos) > 1 {
		return nil, usageErr("usage: hfd menu [YYYY-MM-DD] [--menu-id <id>]")
	}

	// Optional weekday filter. The date is NOT sent anywhere: menus carry no
	// dates, so this only narrows items by weekday (date_binding says so).
	var wantDay string
	if len(pos) == 1 {
		t, ok := parseDate(pos[0])
		if !ok {
			return nil, validationErr("invalid date " + strconv.Quote(pos[0]) + ": expected YYYY-MM-DD")
		}
		wantDay = dayKey(t.Weekday().String())
	}

	query := menuSelect + "&published_at=not.is.null&order=published_at.desc,id.desc&limit=1"
	if menuID != "" {
		if !validUUID(menuID) {
			return nil, validationErr("invalid --menu-id: expected a UUID")
		}
		query = menuSelect + "&id=eq." + menuID + "&limit=1"
	}
	rows, cerr := restRows(d, "menus", query)
	if cerr != nil {
		return nil, cerr
	}
	if len(rows) == 0 {
		msg := "no published menu found"
		if menuID != "" {
			msg = "menu not found"
		}
		return nil, &CLIError{
			Code: CodeRemote, Message: msg, Retryable: false, Status: 200,
			Details: map[string]interface{}{"menu_id": menuID},
		}
	}
	menu := menuSummary(rows[0])
	if menu.ID == "" {
		return nil, &CLIError{
			Code: CodeRemote, Message: "menu row has no id", Retryable: false, Status: 200,
			Details: map[string]interface{}{"rows": len(rows)},
		}
	}

	itemRows, cerr := restRows(d, "menu_items",
		"select=id,day_of_week,category,name,sort_order&menu_id=eq."+menu.ID+"&order=sort_order.asc")
	if cerr != nil {
		return nil, cerr
	}
	items := make([]MenuItem, 0, len(itemRows))
	for _, r := range itemRows {
		it := MenuItem{
			ID:        jsonStr(r, "id"),
			DayOfWeek: jsonStr(r, "day_of_week"),
			Category:  jsonStr(r, "category"),
			Name:      jsonStr(r, "name"),
			SortOrder: jsonInt(r, "sort_order"),
		}
		if wantDay != "" && dayKey(it.DayOfWeek) != wantDay {
			continue
		}
		items = append(items, it)
	}

	return &MenuData{
		MenuID:      menu.ID,
		PublishedAt: menu.PublishedAt,
		DateBinding: dateBindingLabel,
		Items:       items,
	}, nil
}

// dayKey normalises a weekday label to its lowercase 3-letter prefix so that
// "Mon", "monday" and "MONDAY" all compare equal.
func dayKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 3 {
		s = s[:3]
	}
	return s
}

func cmdMenus(d *Deps, a []string) (interface{}, *CLIError) {
	if len(a) > 0 {
		return nil, usageErr("usage: hfd menus")
	}
	rows, cerr := restRows(d, "menus", menuSelect+"&published_at=not.is.null&order=published_at.desc,id.desc")
	if cerr != nil {
		return nil, cerr
	}
	menus := make([]MenuSummary, 0, len(rows))
	for _, r := range rows {
		menus = append(menus, menuSummary(r))
	}
	return &MenusData{Menus: menus}, nil
}

// ---- my-orders (also the reconciliation read) -----------------------------

// fetchOrders reads /functions/v1/my-orders with the personal token.
func fetchOrders(d *Deps) ([]OrderRecord, *CLIError) {
	status, body, err := d.Backend.EdgeGET("my-orders")
	if err != nil {
		return nil, &CLIError{
			Code: CodeRemote, Message: "my-orders read failed", Retryable: true,
			Status: status, Details: map[string]interface{}{"transport": err.Error()},
		}
	}
	if cerr := classifyRead(status, body); cerr != nil {
		return nil, cerr
	}
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, &CLIError{
			Code: CodeRemote, Message: "my-orders returned a non-JSON response", Retryable: false,
			Status: status, Details: remoteDetails(status, body),
		}
	}
	var rows []map[string]interface{}
	switch t := v.(type) {
	case map[string]interface{}:
		rows = objects(t["orders"])
		if rows == nil {
			// Some shapes wrap differently; an object without "orders" is a shape error.
			return nil, &CLIError{
				Code: CodeRemote, Message: "my-orders returned an unexpected response shape", Retryable: false,
				Status: status, Details: remoteDetails(status, body),
			}
		}
	case []interface{}:
		rows = objects(t)
	default:
		return nil, &CLIError{
			Code: CodeRemote, Message: "my-orders returned an unexpected response shape", Retryable: false,
			Status: status, Details: remoteDetails(status, body),
		}
	}
	out := make([]OrderRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, orderRecord(r))
	}
	return out, nil
}

// orderRecord maps a backend order object onto the locked schema. Only order_id
// is guaranteed (R4 #4); everything else stays empty when absent.
func orderRecord(m map[string]interface{}) OrderRecord {
	rec := OrderRecord{
		OrderID:      jsonStr(m, "order_id", "id"),
		Status:       jsonStr(m, "status", "state"),
		MenuItemID:   jsonStr(m, "menu_item_id", "menuItemId"),
		ItemName:     jsonStr(m, "item_name", "name"),
		DeliveryDate: jsonStr(m, "delivery_date", "deliveryDate", "date"),
		OrderName:    jsonStr(m, "order_name", "orderName", "guest_name"),
		CreatedAt:    jsonStr(m, "created_at", "createdAt"),
	}
	// A nested menu_items{name} is the common Supabase join shape.
	if rec.ItemName == "" {
		if mi, ok := m["menu_items"].(map[string]interface{}); ok {
			rec.ItemName = jsonStr(mi, "name")
			if rec.MenuItemID == "" {
				rec.MenuItemID = jsonStr(mi, "id")
			}
		}
	}
	return rec
}

func cmdOrders(d *Deps, a []string) (interface{}, *CLIError) {
	if len(a) > 0 {
		return nil, usageErr("usage: hfd orders")
	}
	recs, cerr := fetchOrders(d)
	if cerr != nil {
		return nil, cerr
	}
	return &OrdersData{Orders: recs}, nil
}

// ---- write protocol (shared by order + cancel; §5, R3 B1/B2) --------------

// fileLock is the advisory lock held across preflight → POST → reconcile.
type fileLock struct{ f *os.File }

func acquireLock() (*fileLock, *CLIError) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, &CLIError{Code: CodeLocked, Message: "cannot resolve home dir for the write lock"}
	}
	dir := filepath.Join(home, ".config", "hfd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &CLIError{Code: CodeLocked, Message: "cannot create ~/.config/hfd for the write lock"}
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &CLIError{Code: CodeLocked, Message: "cannot open the write lock file"}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, &CLIError{
			Code: CodeLocked, Message: "another hfd order/cancel is in progress", Retryable: true,
		}
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}

// classifyWrite maps a non-ambiguous backend write response onto the catalog.
// It returns (nil, true) when the caller must run the reconciliation path.
func classifyWrite(status int, body []byte, transportErr error) (cerr *CLIError, ambiguous bool) {
	if transportErr != nil {
		return nil, true
	}
	if status >= 500 {
		return nil, true
	}
	if status >= 200 && status <= 299 {
		return nil, false
	}
	if status == 401 || status == 403 {
		return &CLIError{
			Code: CodeAuth, Message: "backend rejected the credentials", Retryable: false,
			Status: status, Details: remoteDetails(status, body),
		}, false
	}
	// Coarse cutoff detection only — the backend text is never interpreted further.
	low := strings.ToLower(string(body))
	if strings.Contains(low, "closed") || strings.Contains(low, "cut") {
		return &CLIError{
			Code: CodeOrderingClosed, Message: "the ordering window is closed for this date", Retryable: false,
			Status: status, Details: remoteDetails(status, body),
		}, false
	}
	if status == 400 || status == 422 {
		return &CLIError{
			Code: CodeValidation, Message: fmt.Sprintf("backend rejected the request (status %d)", status),
			Retryable: false, Status: status, Details: remoteDetails(status, body),
		}, false
	}
	return &CLIError{
		Code: CodeRemote, Message: fmt.Sprintf("backend returned status %d", status), Retryable: false,
		Status: status, Details: remoteDetails(status, body),
	}, false
}

// orderObject digs the order object out of a place-order success body.
// A 2xx we cannot read as an order counts as ambiguous, not as success.
func orderObject(body []byte) (map[string]interface{}, bool) {
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, false
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil, false
	}
	candidates := []map[string]interface{}{m}
	if o, ok := m["order"].(map[string]interface{}); ok {
		candidates = append([]map[string]interface{}{o}, candidates...)
	}
	if o, ok := m["data"].(map[string]interface{}); ok {
		candidates = append([]map[string]interface{}{o}, candidates...)
	}
	if arr := objects(m["orders"]); len(arr) == 1 {
		candidates = append([]map[string]interface{}{arr[0]}, candidates...)
	}
	for _, c := range candidates {
		if jsonStr(c, "order_id", "id") != "" {
			return c, true
		}
	}
	return nil, false
}

func orderIDSet(recs []OrderRecord) map[string]bool {
	s := make(map[string]bool, len(recs))
	for _, r := range recs {
		if r.OrderID != "" {
			s[r.OrderID] = true
		}
	}
	return s
}

// sameDate compares delivery dates by their YYYY-MM-DD prefix so that a
// timestamped backend value still matches the requested date.
func sameDate(got, want string) bool {
	if len(got) >= 10 {
		got = got[:10]
	}
	return got == want
}

func isCanceled(status string) bool {
	return strings.Contains(strings.ToLower(status), "cancel")
}

// ---- order ----------------------------------------------------------------

func cmdOrder(d *Deps, a []string) (interface{}, *CLIError) {
	// `--for` (guest ordering) is disabled until live V1 verifies the field.
	for _, arg := range a {
		if arg == "--for" || strings.HasPrefix(arg, "--for=") {
			return nil, validationErr("--for (guest ordering) is disabled pending verification")
		}
	}
	var note string
	pos, cerr := splitArgs(a, map[string]*string{"--note": &note})
	if cerr != nil {
		return nil, cerr
	}
	if len(pos) != 2 {
		return nil, usageErr("usage: hfd order <menu_item_id> <YYYY-MM-DD> [--note <text>]")
	}
	itemID, date := pos[0], pos[1]
	if !validUUID(itemID) {
		return nil, validationErr("invalid menu_item_id: expected a UUID")
	}
	if _, ok := parseDate(date); !ok {
		return nil, validationErr("invalid delivery date " + strconv.Quote(date) + ": expected YYYY-MM-DD")
	}

	lock, cerr := acquireLock()
	if cerr != nil {
		return nil, cerr
	}
	defer lock.release()

	// Snapshot BEFORE the POST; a failed snapshot aborts with zero POSTs.
	before, cerr := fetchOrders(d)
	if cerr != nil {
		cerr.Message = "preflight my-orders read failed, no order was placed: " + cerr.Message
		return nil, cerr
	}
	beforeIDs := orderIDSet(before)

	payload := map[string]string{"menu_item_id": itemID, "delivery_date": date}
	if note != "" {
		payload["special_requirements"] = note // e.g. "spicy very spicy"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &CLIError{Code: CodeValidation, Message: "could not encode the order body"}
	}

	// Exactly one POST. Never retried.
	status, respBody, postErr := d.Backend.EdgePOST("place-order", body)
	cerr, ambiguous := classifyWrite(status, respBody, postErr)
	if cerr != nil {
		return nil, cerr
	}
	if !ambiguous {
		if o, ok := orderObject(respBody); ok {
			rec := orderRecord(o)
			data := &OrderData{
				OrderID:      rec.OrderID,
				Status:       rec.Status,
				MenuItemID:   rec.MenuItemID,
				DeliveryDate: rec.DeliveryDate,
				Self:         true,
			}
			if data.MenuItemID == "" {
				data.MenuItemID = itemID
			}
			if data.DeliveryDate == "" {
				data.DeliveryDate = date
			}
			return data, nil
		}
		// 2xx we cannot read as an order → treat as ambiguous, not as success.
		ambiguous = true
	}

	return reconcileOrder(d, itemID, date, beforeIDs, len(before))
}

// reconcileOrder polls my-orders and reports success only when EXACTLY ONE new
// order matches item + date + self. Anything else is UNKNOWN_OUTCOME.
func reconcileOrder(d *Deps, itemID, date string, beforeIDs map[string]bool, countBefore int) (interface{}, *CLIError) {
	countAfter := countBefore
	var matchIDs []string
	var lastErr *CLIError

	for _, wait := range reconcilePolls {
		time.Sleep(wait)
		after, cerr := fetchOrders(d)
		if cerr != nil {
			lastErr = cerr
			continue
		}
		lastErr = nil
		countAfter = len(after)
		matchIDs = matchIDs[:0]
		for _, r := range after {
			if r.OrderID == "" || beforeIDs[r.OrderID] {
				continue
			}
			// Match a genuinely new order by delivery date. The my-orders list
			// rows expose only {order_id, status, delivery_date} — no
			// menu_item_id — so requiring r.MenuItemID == itemID (as before)
			// never matched and every placement degraded to UNKNOWN_OUTCOME.
			// A non-empty OrderName still marks someone else's (guest) order.
			if !sameDate(r.DeliveryDate, date) || r.OrderName != "" {
				continue
			}
			matchIDs = append(matchIDs, r.OrderID)
		}
		if len(matchIDs) == 1 {
			rec := findOrder(after, matchIDs[0])
			return &OrderData{
				OrderID:      rec.OrderID,
				Status:       rec.Status,
				MenuItemID:   itemID,
				DeliveryDate: date,
				Self:         true,
			}, nil
		}
		if len(matchIDs) > 1 {
			break
		}
	}

	obs := map[string]interface{}{
		"orders_before": countBefore,
		"orders_after":  countAfter,
		"new_match_ids": append([]string{}, matchIDs...),
	}
	if lastErr != nil {
		obs["reconcile_read_failed"] = true
		obs["orders_after"] = nil
	}
	return nil, &CLIError{
		Code:      CodeUnknownOutcome,
		Message:   "the order request was sent but its outcome could not be confirmed; do NOT retry blindly — check `hfd orders`",
		Retryable: false,
		Details: &UnknownOutcomeDetails{
			Operation:          "place-order",
			RequestFingerprint: &Fingerprint{MenuItemID: itemID, DeliveryDate: date},
			Observations:       obs,
		},
	}
}

func findOrder(recs []OrderRecord, id string) OrderRecord {
	for _, r := range recs {
		if r.OrderID == id {
			return r
		}
	}
	return OrderRecord{OrderID: id}
}

// ---- cancel ---------------------------------------------------------------

func cmdCancel(d *Deps, a []string) (interface{}, *CLIError) {
	pos, cerr := splitArgs(a, map[string]*string{})
	if cerr != nil {
		return nil, cerr
	}
	if len(pos) != 1 {
		return nil, usageErr("usage: hfd cancel <order_id>")
	}
	orderID := pos[0]
	if !validUUID(orderID) {
		return nil, validationErr("invalid order_id: expected a UUID")
	}

	lock, cerr := acquireLock()
	if cerr != nil {
		return nil, cerr
	}
	defer lock.release()

	// 1. Preflight. A failed read aborts with zero POSTs.
	before, cerr := fetchOrders(d)
	if cerr != nil {
		cerr.Message = "preflight my-orders read failed, nothing was canceled: " + cerr.Message
		return nil, cerr
	}
	target, present := lookupOrder(before, orderID)
	if !present {
		return &CancelData{OrderID: orderID, FinalState: "absent"}, nil
	}
	if isCanceled(target.Status) {
		return &CancelData{OrderID: orderID, FinalState: "canceled"}, nil
	}

	body, err := json.Marshal(map[string]string{"order_id": orderID})
	if err != nil {
		return nil, &CLIError{Code: CodeValidation, Message: "could not encode the cancel body"}
	}

	// 2. Exactly one POST.
	status, respBody, postErr := d.Backend.EdgePOST("cancel-order", body)
	cerr, ambiguous := classifyWrite(status, respBody, postErr)
	if cerr != nil {
		return nil, cerr
	}
	if !ambiguous {
		// A 2xx whose body is not readable JSON is ambiguous, not success.
		if len(strings.TrimSpace(string(respBody))) == 0 || json.Valid(respBody) {
			return &CancelData{OrderID: orderID, FinalState: "canceled"}, nil
		}
		ambiguous = true
	}

	// 3. Reconcile: absent or status=canceled resolves it; nothing else does.
	var presentAfter interface{}
	for _, wait := range reconcilePolls {
		time.Sleep(wait)
		after, rerr := fetchOrders(d)
		if rerr != nil {
			continue
		}
		rec, still := lookupOrder(after, orderID)
		presentAfter = still
		if !still {
			return &CancelData{OrderID: orderID, FinalState: "absent"}, nil
		}
		if isCanceled(rec.Status) {
			return &CancelData{OrderID: orderID, FinalState: "canceled"}, nil
		}
	}

	return nil, &CLIError{
		Code:      CodeUnknownOutcome,
		Message:   "the cancel request was sent but its outcome could not be confirmed; do NOT retry blindly — check `hfd orders`",
		Retryable: false,
		Details: &UnknownOutcomeDetails{
			Operation: "cancel-order",
			OrderID:   orderID,
			Observations: map[string]interface{}{
				"present_before": true,
				"present_after":  presentAfter,
			},
		},
	}
}

func lookupOrder(recs []OrderRecord, id string) (OrderRecord, bool) {
	for _, r := range recs {
		if r.OrderID == id {
			return r, true
		}
	}
	return OrderRecord{}, false
}

// ---- passthrough: call / get ----------------------------------------------

func cmdCall(d *Deps, a []string) (interface{}, *CLIError) {
	method := "GET"
	pos, cerr := splitArgs(a, map[string]*string{"--method": &method})
	if cerr != nil {
		return nil, cerr
	}
	if len(pos) < 1 || len(pos) > 2 {
		return nil, usageErr("usage: hfd call --method GET|POST <function> [json|-]")
	}
	fn := pos[0]
	method = strings.ToUpper(strings.TrimSpace(method))
	if method != "GET" && method != "POST" {
		return nil, usageErr("--method must be GET or POST")
	}

	var body []byte
	if len(pos) == 2 {
		if method != "POST" {
			return nil, usageErr("a body is only allowed with --method POST")
		}
		if pos[1] == "-" {
			// Stdin keeps secrets out of argv (plan §4).
			b, err := io.ReadAll(io.LimitReader(d.Stdin, maxResponseBytes))
			if err != nil {
				return nil, usageErr("could not read the request body from stdin")
			}
			body = b
		} else {
			body = []byte(pos[1])
		}
	}

	var (
		status int
		resp   []byte
		err    error
	)
	// No validation here at all: `call` is the full-access escape hatch.
	if method == "GET" {
		status, resp, err = d.Backend.EdgeGET(fn)
	} else {
		if body == nil {
			body = []byte("{}")
		}
		status, resp, err = d.Backend.EdgePOST(fn, body)
	}
	if err != nil {
		return nil, &CLIError{
			Code: CodeRemote, Message: "request failed", Retryable: true,
			Status: status, Details: map[string]interface{}{"transport": err.Error()},
		}
	}
	// A non-2xx is data here, not an error: surface it with its status.
	return &Passthrough{Passthrough: true, HTTPStatus: status, Body: decodeJSON(resp)}, nil
}

func cmdGet(d *Deps, a []string) (interface{}, *CLIError) {
	pos, cerr := splitArgs(a, map[string]*string{})
	if cerr != nil {
		return nil, cerr
	}
	if len(pos) < 1 || len(pos) > 2 {
		return nil, usageErr("usage: hfd get <table> [querystring]")
	}
	query := ""
	if len(pos) == 2 {
		query = pos[1]
	}
	status, resp, err := d.Backend.RestGET(pos[0], query)
	if err != nil {
		return nil, &CLIError{
			Code: CodeRemote, Message: "request failed", Retryable: true,
			Status: status, Details: map[string]interface{}{"transport": err.Error()},
		}
	}
	return &Passthrough{Passthrough: true, HTTPStatus: status, Body: decodeJSON(resp)}, nil
}

// ---- next -----------------------------------------------------------------

// cmdNext returns the Mon–Fri of the week AFTER the current one.
// Rule (deliberately simple, calendar-only): the current week is already closed
// for ordering, so "next week" = current week's Monday + 7 days, then +0..4.
// Saturday and Sunday belong to the current (ISO) week, so on a weekend this
// still returns the Monday 2 or 1 days away. These dates are HINTS ONLY —
// authoritative:false — the server owns the real ordering window.
func cmdNext(d *Deps, a []string) (interface{}, *CLIError) {
	if len(a) > 0 {
		return nil, usageErr("usage: hfd next")
	}
	now := d.Clock.Now().In(SAST)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, SAST)
	daysSinceMonday := (int(today.Weekday()) + 6) % 7 // Sunday(0) → 6
	nextMonday := today.AddDate(0, 0, -daysSinceMonday+7)

	dates := make([]NextDate, 0, 5)
	for i := 0; i < 5; i++ {
		day := nextMonday.AddDate(0, 0, i)
		dates = append(dates, NextDate{
			Date:    day.Format("2006-01-02"),
			Weekday: day.Weekday().String(),
		})
	}
	return &NextData{Authoritative: false, Dates: dates}, nil
}

// ---- help (single source of truth; §6 + R4) -------------------------------

const helpBody = `hfd — Home Food Deli CLI

JSON is the default output. Every machine command emits exactly one JSON
document: {"version":1,"ok":true,"data":...} or
{"version":1,"ok":false,"error":{"code","message","retryable","status?","details?"}}.
Exit code is 0 only on ok:true (64 on USAGE, 2 otherwise).

USAGE
  hfd <command> [args] [flags]
  hfd help | hfd help --json | hfd <command> --help

COMMANDS
  menu [YYYY-MM-DD] [--menu-id <id>]   Items for a menu. Default = newest published menu
                                       (a labeled heuristic). A date filters by WEEKDAY only.
  menus                                List published menus (id, year, quarter, week, published_at).
  order <item_id> <YYYY-MM-DD> [--note <text>]  Place a self-order. --note sets special
                                       requirements (e.g. "spicy very spicy"). --for is DISABLED.
  orders                               List my orders.
  cancel <order_id>                    Cancel an order (preflight + reconciliation).
  call --method GET|POST <fn> [json|-] Generic edge-function passthrough; "-" reads the body from stdin.
  get <table> [querystring]            Generic PostgREST read via the anon key.
  next                                 Next week's Mon–Fri dates. Hints only (authoritative:false).
  help [--json]                        This text, or the machine-readable catalog.

ERROR CODES
  USAGE, VALIDATION, AUTH, REMOTE, ORDERING_CLOSED, LOCKED, UNKNOWN_OUTCOME

  UNKNOWN_OUTCOME means the write was sent but its result could not be confirmed.
  Never auto-retry it: read ` + "`hfd orders`" + ` and decide.

TOKEN SETUP
  The personal token authenticates every edge-function call (order, orders, cancel, call).
  PostgREST reads (menu, menus, get) use the public anon key and never carry the token.

    export HFD_TOKEN=hfd_xxx
  or
    mkdir -p ~/.config/hfd && printf 'hfd_xxx' > ~/.config/hfd/token && chmod 600 ~/.config/hfd/token

  A token file with looser permissions than 0600 is refused.

NOTES
  Dates are interpreted in SAST (UTC+2). Menus carry no dates, so the CLI never
  claims an item is orderable for a date — the server owns that verdict.
  order and cancel take an advisory lock (~/.config/hfd/lock); a second concurrent
  run fails with LOCKED and sends no request.
`

func helpText() string { return helpBody }

func cmdHelpText(cmd string) string {
	for _, c := range helpCommands() {
		if c.Name == cmd {
			b := &strings.Builder{}
			fmt.Fprintf(b, "hfd %s", c.Name)
			for _, a := range c.Args {
				fmt.Fprintf(b, " %s", a)
			}
			for _, f := range c.Flags {
				fmt.Fprintf(b, " [%s]", f)
			}
			fmt.Fprintf(b, "\n\n  %s\n", c.Description)
			if len(c.Notes) > 0 {
				fmt.Fprintf(b, "\n")
				for _, n := range c.Notes {
					fmt.Fprintf(b, "  - %s\n", n)
				}
			}
			return b.String()
		}
	}
	return "unknown command: " + cmd + "\n\n" + helpBody
}

// helpCommand is one entry of the machine catalog (help --json).
type helpCommand struct {
	Name        string   `json:"name"`
	Args        []string `json:"args"`
	Flags       []string `json:"flags"`
	Description string   `json:"description"`
	Notes       []string `json:"notes,omitempty"`
}

type helpCatalog struct {
	Version    int           `json:"version"`
	BaseURL    string        `json:"base_url"`
	Functions  []string      `json:"functions"`
	ErrorCodes []string      `json:"error_codes"`
	Envelope   helpEnvelope  `json:"envelope"`
	Commands   []helpCommand `json:"commands"`
}

type helpEnvelope struct {
	Description string `json:"description"`
	Success     string `json:"success"`
	Error       string `json:"error"`
	ExitCodes   string `json:"exit_codes"`
}

func helpCommands() []helpCommand {
	return []helpCommand{
		{
			Name: "menu", Args: []string{"[YYYY-MM-DD]"}, Flags: []string{"--menu-id <id>"},
			Description: "Items for a menu; default is the newest published menu (heuristic, labeled).",
			Notes: []string{
				"A date argument filters by WEEKDAY only; data.date_binding says so.",
				"Menus carry no dates, so this never asserts an item is orderable for a date.",
			},
		},
		{
			Name: "menus", Args: []string{}, Flags: []string{},
			Description: "List published menus, newest published_at first.",
		},
		{
			Name: "order", Args: []string{"<menu_item_id>", "<YYYY-MM-DD>"}, Flags: []string{"--note <text>"},
			Description: "Place a self-order for a menu item on a delivery date.",
			Notes: []string{
				"--for (guest ordering) is DISABLED and returns VALIDATION.",
				"Structural validation only: UUID + YYYY-MM-DD. The server owns the cutoff verdict.",
				"Exactly one POST, never auto-retried; ambiguity returns UNKNOWN_OUTCOME.",
			},
		},
		{
			Name: "orders", Args: []string{}, Flags: []string{},
			Description: "List my orders (also the reconciliation read for order/cancel).",
		},
		{
			Name: "cancel", Args: []string{"<order_id>"}, Flags: []string{},
			Description: "Cancel an order. final_state is \"canceled\" or \"absent\" only.",
			Notes: []string{
				"Preflight my-orders first: an absent or already-canceled order sends no POST.",
				"An unresolved cancel is UNKNOWN_OUTCOME, never a data state.",
			},
		},
		{
			Name: "call", Args: []string{"<function>", "[json|-]"}, Flags: []string{"--method GET|POST"},
			Description: "Generic edge-function passthrough; bypasses all validation.",
			Notes: []string{
				"Body arg \"-\" reads the JSON body from stdin (keeps secrets out of argv).",
				"A non-2xx is returned as data: {passthrough:true, http_status, body}.",
			},
		},
		{
			Name: "get", Args: []string{"<table>", "[querystring]"}, Flags: []string{},
			Description: "Generic PostgREST read with the anon key; bypasses all validation.",
		},
		{
			Name: "next", Args: []string{}, Flags: []string{},
			Description: "Next week's Monday–Friday dates in SAST. Hints only (authoritative:false).",
		},
		{
			Name: "help", Args: []string{"[--json]"}, Flags: []string{},
			Description: "Human help text, or this machine-readable catalog.",
		},
	}
}

func helpJSON() interface{} {
	return helpCatalog{
		Version:   contractVersion,
		BaseURL:   BaseURL,
		Functions: []string{"place-order", "cancel-order", "my-orders"},
		ErrorCodes: []string{
			string(CodeUsage), string(CodeValidation), string(CodeAuth), string(CodeRemote),
			string(CodeOrderingClosed), string(CodeLocked), string(CodeUnknownOutcome),
		},
		Envelope: helpEnvelope{
			Description: "Exactly one JSON document per machine invocation; help output is plain text instead.",
			Success:     `{"version":1,"ok":true,"data":<command-specific>}`,
			Error:       `{"version":1,"ok":false,"error":{"code","message","retryable","status?","details?"}}`,
			ExitCodes:   "0 on ok:true, 64 on USAGE, 2 on any other error",
		},
		Commands: helpCommands(),
	}
}
