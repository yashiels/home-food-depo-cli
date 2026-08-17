package main

// order_test.go — the write protocol for `order` (plan §5, R3 B1/B6, R4 #1).
//
// The invariant under test throughout: a request that the CLI itself rejects,
// or that it cannot safely place, results in ZERO POSTs; a request it does
// place results in EXACTLY ONE POST, never retried.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ---- 1. rejected input → zero POSTs ---------------------------------------

func TestOrderRejectedInputSendsNoPost(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"bad uuid", []string{"not-a-uuid", deliverDate}},
		{"uuid missing groups", []string{"1111-2222-3333", deliverDate}},
		{"impossible calendar date", []string{itemUUID, "2026-02-31"}},
		{"date wrong shape", []string{itemUUID, "16/03/2026"}},
		{"date with time", []string{itemUUID, "2026-03-16T00:00:00Z"}},
		{"--for guest flag", []string{itemUUID, deliverDate, "--for", "Alice"}},
		{"--for=guest flag", []string{itemUUID, deliverDate, "--for=Alice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fb := &fakeBackend{}
			data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), tc.args)

			requireCLIError(t, data, err, CodeValidation)
			requirePostCount(t, fb, 0)
			if fb.getCount != 0 {
				t.Fatalf("rejected input still read my-orders (%d EdgeGETs)", fb.getCount)
			}
		})
	}
}

func TestOrderUsageErrorsSendNoPost(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"one positional", []string{itemUUID}},
		{"three positionals", []string{itemUUID, deliverDate, "extra"}},
		{"unknown flag", []string{"--menu-id", menuUUID, itemUUID, deliverDate}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fb := &fakeBackend{}
			data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), tc.args)

			requireCLIError(t, data, err, CodeUsage)
			requirePostCount(t, fb, 0)
		})
	}
}

// ---- 2. clean success → exactly one POST ----------------------------------

func TestOrderHappyPathPostsExactlyOnce(t *testing.T) {
	isolateHome(t)

	placed := orderRow(newOrderID, itemUUID, deliverDate, "confirmed")
	fb := &fakeBackend{
		// Snapshot before the POST is empty; afterwards it holds the new order.
		edgeGetFunc:  sequenceOrders(ordersBody(), ordersBody(placed)),
		edgePostFunc: statusPost(200, mustJSON(placed)),
	}

	data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), []string{itemUUID, deliverDate})
	requireNoError(t, err)
	requirePostCount(t, fb, 1)

	if fb.postCalls[0].Fn != "place-order" {
		t.Fatalf("posted to %q, want place-order", fb.postCalls[0].Fn)
	}
	od, ok := data.(*OrderData)
	if !ok {
		t.Fatalf("data is %T, want *OrderData", data)
	}
	if od.OrderID != newOrderID {
		t.Fatalf("order_id = %q, want %q", od.OrderID, newOrderID)
	}
	if !od.Self {
		t.Fatalf("self = false, want true")
	}
	if od.MenuItemID != itemUUID || od.DeliveryDate != deliverDate {
		t.Fatalf("echoed request wrong: %+v", od)
	}
	if fb.getCount != 1 {
		t.Fatalf("EdgeGET count = %d, want 1 (preflight only on a clean success)", fb.getCount)
	}
}

// ---- 3. ambiguous write → UNKNOWN_OUTCOME, no retry -----------------------

func TestOrderAmbiguousWriteIsUnknownOutcome(t *testing.T) {
	existing := orderRow(altOrderID, otherUUID, "2026-03-17", "confirmed")
	twinA := orderRow(newOrderID, itemUUID, deliverDate, "confirmed")
	twinB := orderRow(altOrderID+"0", itemUUID, deliverDate, "confirmed")

	cases := []struct {
		name string
		post edgePostFn
		get  edgeGetFn
	}{
		{
			name: "500 with zero matches",
			post: statusPost(500, []byte(`{"error":"internal"}`)),
			get:  sequenceOrders(ordersBody(existing), ordersBody(existing)),
		},
		{
			name: "transport error with zero matches",
			post: func(string, []byte) (int, []byte, error) {
				return 0, nil, errors.New("dial tcp: i/o timeout")
			},
			get: sequenceOrders(ordersBody(), ordersBody()),
		},
		{
			name: "500 with many matches",
			post: statusPost(502, []byte(`bad gateway`)),
			get:  sequenceOrders(ordersBody(), ordersBody(twinA, twinB)),
		},
		{
			name: "2xx body that is not an order",
			post: statusPost(200, []byte(`{"ok":true}`)),
			get:  sequenceOrders(ordersBody(), ordersBody()),
		},
		{
			name: "reconcile reads all fail",
			post: statusPost(503, []byte(`unavailable`)),
			get: func() edgeGetFn {
				first := true
				return func(string) (int, []byte, error) {
					if first {
						first = false
						return 200, ordersBody(), nil
					}
					return 0, nil, errors.New("dial tcp: connection reset by peer")
				}
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fb := &fakeBackend{edgeGetFunc: tc.get, edgePostFunc: tc.post}

			data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), []string{itemUUID, deliverDate})
			cerr := requireCLIError(t, data, err, CodeUnknownOutcome)

			if cerr.Retryable {
				t.Fatalf("UNKNOWN_OUTCOME must be retryable:false")
			}
			requirePostCount(t, fb, 1) // sent once, never replayed

			det, ok := cerr.Details.(*UnknownOutcomeDetails)
			if !ok {
				t.Fatalf("details is %T, want *UnknownOutcomeDetails", cerr.Details)
			}
			if det.Operation != "place-order" {
				t.Fatalf("operation = %q, want place-order", det.Operation)
			}
			if det.RequestFingerprint == nil {
				t.Fatalf("request_fingerprint missing")
			}
			if det.RequestFingerprint.MenuItemID != itemUUID || det.RequestFingerprint.DeliveryDate != deliverDate {
				t.Fatalf("fingerprint = %+v", det.RequestFingerprint)
			}
			if det.Observations == nil {
				t.Fatalf("observations missing")
			}
		})
	}
}

// Exactly one new match after an ambiguous POST resolves to SUCCESS.
func TestOrderAmbiguousWithExactlyOneMatchResolvesToSuccess(t *testing.T) {
	isolateHome(t)

	existing := orderRow(altOrderID, otherUUID, "2026-03-17", "confirmed")
	placed := orderRow(newOrderID, itemUUID, deliverDate, "confirmed")

	fb := &fakeBackend{
		edgeGetFunc:  sequenceOrders(ordersBody(existing), ordersBody(existing, placed)),
		edgePostFunc: statusPost(500, []byte(`{"error":"gateway timeout"}`)),
	}

	data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), []string{itemUUID, deliverDate})
	requireNoError(t, err)
	requirePostCount(t, fb, 1)

	od, ok := data.(*OrderData)
	if !ok {
		t.Fatalf("data is %T, want *OrderData", data)
	}
	if od.OrderID != newOrderID {
		t.Fatalf("reconciled order_id = %q, want %q", od.OrderID, newOrderID)
	}
	if !od.Self || od.MenuItemID != itemUUID || od.DeliveryDate != deliverDate {
		t.Fatalf("reconciled order = %+v", od)
	}
}

// A guest order (order_name set) is NOT counted as our self-order match.
func TestOrderReconcileIgnoresGuestAndMismatchedRows(t *testing.T) {
	isolateHome(t)

	guest := orderRow(newOrderID, itemUUID, deliverDate, "confirmed")
	guest["order_name"] = "Someone Else"
	wrongDate := orderRow(altOrderID, itemUUID, "2026-03-17", "confirmed")

	fb := &fakeBackend{
		edgeGetFunc:  sequenceOrders(ordersBody(), ordersBody(guest, wrongDate)),
		edgePostFunc: statusPost(500, []byte(`boom`)),
	}

	data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), []string{itemUUID, deliverDate})
	requireCLIError(t, data, err, CodeUnknownOutcome)
	requirePostCount(t, fb, 1)
}

// A timestamped delivery_date still matches the requested calendar date.
func TestOrderReconcileMatchesTimestampedDeliveryDate(t *testing.T) {
	isolateHome(t)

	placed := orderRow(newOrderID, itemUUID, deliverDate+"T00:00:00+02:00", "confirmed")
	fb := &fakeBackend{
		edgeGetFunc:  sequenceOrders(ordersBody(), ordersBody(placed)),
		edgePostFunc: statusPost(500, []byte(`boom`)),
	}

	data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), []string{itemUUID, deliverDate})
	requireNoError(t, err)
	if od := data.(*OrderData); od.OrderID != newOrderID {
		t.Fatalf("order_id = %q", od.OrderID)
	}
}

// ---- 4. snapshot failure → zero POST --------------------------------------

func TestOrderSnapshotFailureSendsNoPost(t *testing.T) {
	cases := []struct {
		name string
		get  edgeGetFn
		want ErrCode
	}{
		{"transport failure", failingGet(), CodeRemote},
		{
			name: "auth rejected",
			get:  func(string) (int, []byte, error) { return 401, []byte(`{"message":"invalid token"}`), nil },
			want: CodeAuth,
		},
		{
			name: "non-JSON body",
			get:  func(string) (int, []byte, error) { return 200, []byte(`<html>gateway</html>`), nil },
			want: CodeRemote,
		},
		{
			name: "unexpected shape",
			get:  func(string) (int, []byte, error) { return 200, []byte(`{"data":[]}`), nil },
			want: CodeRemote,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fb := &fakeBackend{edgeGetFunc: tc.get}

			data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), []string{itemUUID, deliverDate})
			cerr := requireCLIError(t, data, err, tc.want)
			requirePostCount(t, fb, 0)

			if !strings.Contains(cerr.Message, "no order was placed") {
				t.Fatalf("message must state nothing was placed, got %q", cerr.Message)
			}
		})
	}
}

// ---- 6. concurrent intent → LOCKED, zero POST -----------------------------

func TestOrderConcurrentIntentIsLocked(t *testing.T) {
	home := isolateHome(t)
	release := readLock(t, home) // a competing hfd holds the lock
	defer release()

	fb := &fakeBackend{}
	data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), []string{itemUUID, deliverDate})

	cerr := requireCLIError(t, data, err, CodeLocked)
	if !cerr.Retryable {
		t.Fatalf("LOCKED should be retryable:true")
	}
	requirePostCount(t, fb, 0)
	if fb.getCount != 0 {
		t.Fatalf("LOCKED run still read my-orders (%d EdgeGETs)", fb.getCount)
	}
}

func TestCancelConcurrentIntentIsLocked(t *testing.T) {
	home := isolateHome(t)
	release := readLock(t, home)
	defer release()

	fb := &fakeBackend{}
	data, err := cmdCancel(newTestDeps(fb, time.Time{}, ""), []string{newOrderID})

	requireCLIError(t, data, err, CodeLocked)
	requirePostCount(t, fb, 0)
}

// The lock must be released again, so a sequential second run succeeds.
func TestOrderLockIsReleasedAfterRun(t *testing.T) {
	isolateHome(t)
	placed := orderRow(newOrderID, itemUUID, deliverDate, "confirmed")

	for i := 0; i < 2; i++ {
		fb := &fakeBackend{
			edgeGetFunc:  sequenceOrders(ordersBody()),
			edgePostFunc: statusPost(200, mustJSON(placed)),
		}
		if _, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), []string{itemUUID, deliverDate}); err != nil {
			t.Fatalf("run %d failed: %s: %s", i, err.Code, err.Message)
		}
	}
}

// ---- 7. ORDERING_CLOSED classification ------------------------------------

func TestOrderClassifiesStableBackendRejections(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrCode
	}{
		{"closed window", 400, `{"error":"ordering is closed for this date"}`, CodeOrderingClosed},
		{"cutoff wording", 422, `{"error":"past the cut-off time"}`, CodeOrderingClosed},
		{"closed on 403 still auth", 403, `{"error":"closed"}`, CodeAuth},
		{"plain validation", 400, `{"error":"menu_item_id is required"}`, CodeValidation},
		{"unprocessable", 422, `{"error":"delivery_date is in the past"}`, CodeValidation},
		{"other remote", 418, `{"error":"teapot"}`, CodeRemote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fb := &fakeBackend{
				edgeGetFunc:  sequenceOrders(ordersBody()),
				edgePostFunc: statusPost(tc.status, []byte(tc.body)),
			}

			data, err := cmdOrder(newTestDeps(fb, time.Time{}, ""), []string{itemUUID, deliverDate})
			cerr := requireCLIError(t, data, err, tc.want)

			requirePostCount(t, fb, 1) // stable answer: sent once, not reconciled
			if cerr.Retryable {
				t.Fatalf("%s should not be retryable", tc.want)
			}
			if cerr.Status != tc.status {
				t.Fatalf("status = %d, want %d", cerr.Status, tc.status)
			}
			if cerr.Details == nil {
				t.Fatalf("REMOTE-class errors must carry details")
			}
		})
	}
}
