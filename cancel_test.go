package main

// cancel_test.go — the cancel state machine (R3 B2, R4 #1/#2).
//
// Deliberately NOT the order state machine: cancel resolves on "target absent
// OR status=canceled", never on "exactly one new match", and final_state is
// only ever "canceled" or "absent" — an unresolved cancel is an error, not a
// data state.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func cancelData(t *testing.T, data interface{}) *CancelData {
	t.Helper()
	cd, ok := data.(*CancelData)
	if !ok {
		t.Fatalf("data is %T, want *CancelData", data)
	}
	if cd.FinalState != "canceled" && cd.FinalState != "absent" {
		t.Fatalf("final_state = %q, must be canceled|absent only", cd.FinalState)
	}
	return cd
}

// ---- 5a/5b. preflight resolves without any POST ---------------------------

func TestCancelPreflightResolvesWithoutPost(t *testing.T) {
	other := orderRow(altOrderID, otherUUID, deliverDate, "confirmed")

	cases := []struct {
		name      string
		snapshot  []byte
		wantState string
	}{
		{"target absent from an empty list", ordersBody(), "absent"},
		{"target absent among others", ordersBody(other), "absent"},
		{
			name:      "already canceled",
			snapshot:  ordersBody(orderRow(newOrderID, itemUUID, deliverDate, "canceled")),
			wantState: "canceled",
		},
		{
			name:      "already canceled, mixed case wording",
			snapshot:  ordersBody(orderRow(newOrderID, itemUUID, deliverDate, "CANCELLED_BY_USER")),
			wantState: "canceled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fb := &fakeBackend{edgeGetFunc: sequenceOrders(tc.snapshot)}

			data, err := cmdCancel(newTestDeps(fb, time.Time{}, ""), []string{newOrderID})
			requireNoError(t, err)
			requirePostCount(t, fb, 0)

			cd := cancelData(t, data)
			if cd.FinalState != tc.wantState {
				t.Fatalf("final_state = %q, want %q", cd.FinalState, tc.wantState)
			}
			if cd.OrderID != newOrderID {
				t.Fatalf("order_id = %q", cd.OrderID)
			}
		})
	}
}

// ---- 5c. happy cancel: present + active → exactly one POST ----------------

func TestCancelHappyPathPostsExactlyOnce(t *testing.T) {
	active := orderRow(newOrderID, itemUUID, deliverDate, "confirmed")
	canceled := orderRow(newOrderID, itemUUID, deliverDate, "canceled")

	cases := []struct {
		name string
		post edgePostFn
		get  edgeGetFn
	}{
		{
			name: "2xx acknowledged directly",
			post: statusPost(200, []byte(`{"ok":true}`)),
			get:  sequenceOrders(ordersBody(active), ordersBody(canceled)),
		},
		{
			name: "2xx with an empty body",
			post: statusPost(204, nil),
			get:  sequenceOrders(ordersBody(active), ordersBody(canceled)),
		},
		{
			name: "ambiguous 5xx, reconciled as canceled",
			post: statusPost(500, []byte(`{"error":"internal"}`)),
			get:  sequenceOrders(ordersBody(active), ordersBody(canceled)),
		},
		{
			name: "ambiguous 5xx, reconciled as absent",
			post: statusPost(502, []byte(`bad gateway`)),
			get:  sequenceOrders(ordersBody(active), ordersBody()),
		},
		{
			name: "transport error, reconciled as canceled",
			post: func(string, []byte) (int, []byte, error) {
				return 0, nil, errors.New("dial tcp: i/o timeout")
			},
			get: sequenceOrders(ordersBody(active), ordersBody(canceled)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fb := &fakeBackend{edgeGetFunc: tc.get, edgePostFunc: tc.post}

			data, err := cmdCancel(newTestDeps(fb, time.Time{}, ""), []string{newOrderID})
			requireNoError(t, err)
			requirePostCount(t, fb, 1)

			if fb.postCalls[0].Fn != "cancel-order" {
				t.Fatalf("posted to %q, want cancel-order", fb.postCalls[0].Fn)
			}
			if !strings.Contains(string(fb.postCalls[0].Body), newOrderID) {
				t.Fatalf("cancel body missing order_id: %s", fb.postCalls[0].Body)
			}
			cancelData(t, data)
		})
	}
}

// ---- 5d. ambiguous cancel → UNKNOWN_OUTCOME -------------------------------

func TestCancelAmbiguousIsUnknownOutcome(t *testing.T) {
	active := orderRow(newOrderID, itemUUID, deliverDate, "confirmed")

	cases := []struct {
		name string
		post edgePostFn
		get  edgeGetFn
	}{
		{
			name: "5xx and the order stays active",
			post: statusPost(500, []byte(`{"error":"internal"}`)),
			get:  sequenceOrders(ordersBody(active)),
		},
		{
			name: "transport error and the order stays active",
			post: func(string, []byte) (int, []byte, error) {
				return 0, nil, errors.New("connection reset by peer")
			},
			get: sequenceOrders(ordersBody(active)),
		},
		{
			name: "2xx with an unreadable body and the order stays active",
			post: statusPost(200, []byte(`<html>oops</html>`)),
			get:  sequenceOrders(ordersBody(active)),
		},
		{
			name: "reconcile reads all fail",
			post: statusPost(503, []byte(`unavailable`)),
			get: func() edgeGetFn {
				first := true
				return func(string) (int, []byte, error) {
					if first {
						first = false
						return 200, ordersBody(active), nil
					}
					return 0, nil, errors.New("dial tcp: connection refused")
				}
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fb := &fakeBackend{edgeGetFunc: tc.get, edgePostFunc: tc.post}

			data, err := cmdCancel(newTestDeps(fb, time.Time{}, ""), []string{newOrderID})
			cerr := requireCLIError(t, data, err, CodeUnknownOutcome)

			if cerr.Retryable {
				t.Fatalf("UNKNOWN_OUTCOME must be retryable:false")
			}
			requirePostCount(t, fb, 1)

			det, ok := cerr.Details.(*UnknownOutcomeDetails)
			if !ok {
				t.Fatalf("details is %T, want *UnknownOutcomeDetails", cerr.Details)
			}
			if det.Operation != "cancel-order" {
				t.Fatalf("operation = %q, want cancel-order", det.Operation)
			}
			if det.OrderID != newOrderID {
				t.Fatalf("details.order_id = %q, want %q", det.OrderID, newOrderID)
			}
			if det.RequestFingerprint != nil {
				t.Fatalf("cancel details must not carry a place-order fingerprint")
			}
			// The removed "unknown" data state must never reappear.
			if strings.Contains(string(mustJSON(cerr.toEnvelope())), `"final_state"`) {
				t.Fatalf("an unresolved cancel leaked a final_state")
			}
		})
	}
}

// ---- cancel validation + preflight failure --------------------------------

func TestCancelRejectedInputSendsNoPost(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want ErrCode
	}{
		{"bad uuid", []string{"nope"}, CodeValidation},
		{"no args", nil, CodeUsage},
		{"two positionals", []string{newOrderID, altOrderID}, CodeUsage},
		{"unknown flag", []string{"--force", "x", newOrderID}, CodeUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fb := &fakeBackend{}
			data, err := cmdCancel(newTestDeps(fb, time.Time{}, ""), tc.args)

			requireCLIError(t, data, err, tc.want)
			requirePostCount(t, fb, 0)
			if fb.getCount != 0 {
				t.Fatalf("rejected input still read my-orders")
			}
		})
	}
}

func TestCancelPreflightFailureSendsNoPost(t *testing.T) {
	isolateHome(t)
	fb := &fakeBackend{edgeGetFunc: failingGet()}

	data, err := cmdCancel(newTestDeps(fb, time.Time{}, ""), []string{newOrderID})
	cerr := requireCLIError(t, data, err, CodeRemote)
	requirePostCount(t, fb, 0)

	if !strings.Contains(cerr.Message, "nothing was canceled") {
		t.Fatalf("message must state nothing was canceled, got %q", cerr.Message)
	}
}
