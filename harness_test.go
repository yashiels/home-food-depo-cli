package main

// harness_test.go — the shared, hermetic test rig.
//
// Nothing here touches the network, the real clock, the real home dir or the
// real wall clock: every test drives the handlers through fakeBackend +
// fakeClock, with HOME redirected to t.TempDir() and reconcilePolls shrunk to
// zero so the reconciliation loops run instantly.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func flockEx(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
func flockUn(f *os.File)       { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// ---- fakeBackend ----------------------------------------------------------

type edgeGetFn func(fn string) (int, []byte, error)
type edgePostFn func(fn string, body []byte) (int, []byte, error)
type restGetFn func(table, query string) (int, []byte, error)

type postCall struct {
	Fn   string
	Body []byte
}

type restCall struct {
	Table string
	Query string
}

// fakeBackend is a programmable, counting Backend.
type fakeBackend struct {
	edgeGetFunc  edgeGetFn
	edgePostFunc edgePostFn
	restGetFunc  restGetFn

	getCount  int // EdgeGET calls
	postCount int // EdgePOST calls
	rstCount  int // RestGET calls

	getCalls  []string
	postCalls []postCall
	restCalls []restCall
}

var _ Backend = (*fakeBackend)(nil)

func (f *fakeBackend) EdgeGET(fn string) (int, []byte, error) {
	f.getCount++
	f.getCalls = append(f.getCalls, fn)
	if f.edgeGetFunc == nil {
		return 200, mustJSON(map[string]interface{}{"orders": []interface{}{}}), nil
	}
	return f.edgeGetFunc(fn)
}

func (f *fakeBackend) EdgePOST(fn string, body []byte) (int, []byte, error) {
	f.postCount++
	f.postCalls = append(f.postCalls, postCall{Fn: fn, Body: append([]byte(nil), body...)})
	if f.edgePostFunc == nil {
		return 200, mustJSON(map[string]interface{}{"order_id": newOrderID}), nil
	}
	return f.edgePostFunc(fn, body)
}

func (f *fakeBackend) RestGET(table, query string) (int, []byte, error) {
	f.rstCount++
	f.restCalls = append(f.restCalls, restCall{Table: table, Query: query})
	if f.restGetFunc == nil {
		return 200, []byte("[]"), nil
	}
	return f.restGetFunc(table, query)
}

// ---- fakeClock ------------------------------------------------------------

type fakeClock struct{ t time.Time }

var _ Clock = fakeClock{}

func (c fakeClock) Now() time.Time { return c.t }

// ---- deps builder ---------------------------------------------------------

// newTestDeps wires a Deps around the fake backend. Passing a zero fakeClock
// time is fine for every command except `next`.
func newTestDeps(fb *fakeBackend, now time.Time, stdin string) *Deps {
	return &Deps{
		Backend: fb,
		Clock:   fakeClock{t: now},
		Token:   "hfd_test",
		Stdin:   strings.NewReader(stdin),
	}
}

// ---- environment isolation ------------------------------------------------

// isolateHome points the advisory lock (~/.config/hfd/lock) at a temp dir and
// shrinks the reconciliation poll schedule so no test ever sleeps. It returns
// the temp home.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Belt and braces for non-darwin/linux resolution paths.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("USERPROFILE", home)
	shrinkPolls(t)
	return home
}

// shrinkPolls replaces the ambiguity poll delays with zero-length waits.
func shrinkPolls(t *testing.T) {
	t.Helper()
	orig := reconcilePolls
	reconcilePolls = []time.Duration{0, 0, 0}
	t.Cleanup(func() { reconcilePolls = orig })
}

// ---- fixtures / small helpers ---------------------------------------------

const (
	itemUUID    = "11111111-2222-4333-8444-555555555555"
	otherUUID   = "99999999-8888-4777-8666-555555555555"
	newOrderID  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	altOrderID  = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	menuUUID    = "12121212-3434-4565-8787-909090909090"
	deliverDate = "2026-03-16"
)

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// orderRow builds a my-orders row in the backend's shape.
// orderRow models a my-orders LIST row, which exposes only
// {order_id, delivery_date, status} — NOT menu_item_id (that appears only on the
// place-order response). itemID is accepted for call-site clarity but
// deliberately not emitted, so reconcile tests prove matching works without it.
func orderRow(id, itemID, date, status string) map[string]interface{} {
	_ = itemID
	return map[string]interface{}{
		"order_id":      id,
		"delivery_date": date,
		"status":        status,
	}
}

// ordersBody wraps rows in the `{"orders":[...]}` envelope my-orders returns.
func ordersBody(rows ...map[string]interface{}) []byte {
	list := make([]interface{}, 0, len(rows))
	for _, r := range rows {
		list = append(list, r)
	}
	return mustJSON(map[string]interface{}{"orders": list})
}

// sequenceOrders returns an EdgeGET stub that serves one my-orders snapshot per
// call, clamping to the final snapshot once the sequence is exhausted. This is
// how a test says "before the POST it looked like X, afterwards like Y".
func sequenceOrders(snapshots ...[]byte) edgeGetFn {
	i := 0
	return func(fn string) (int, []byte, error) {
		b := snapshots[len(snapshots)-1]
		if i < len(snapshots) {
			b = snapshots[i]
		}
		i++
		return 200, b, nil
	}
}

// errNoNetwork stands in for any transport-level failure.
var errNoNetwork = errors.New("dial tcp: connection refused")

// failingGet is an EdgeGET stub that always reports a transport failure.
func failingGet() edgeGetFn {
	return func(string) (int, []byte, error) { return 0, nil, errNoNetwork }
}

// statusPost returns an EdgePOST stub with a fixed status + body.
func statusPost(status int, body []byte) edgePostFn {
	return func(string, []byte) (int, []byte, error) { return status, body, nil }
}

// ---- assertions -----------------------------------------------------------

func requireCLIError(t *testing.T, data interface{}, err *CLIError, want ErrCode) *CLIError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got data %#v", want, data)
	}
	if err.Code != want {
		t.Fatalf("expected code %s, got %s (%s)", want, err.Code, err.Message)
	}
	if data != nil {
		t.Fatalf("expected nil data alongside %s, got %#v", want, data)
	}
	return err
}

func requireNoError(t *testing.T, err *CLIError) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %s: %s", err.Code, err.Message)
	}
}

func requirePostCount(t *testing.T, fb *fakeBackend, want int) {
	t.Helper()
	if fb.postCount != want {
		t.Fatalf("postCount = %d, want %d (calls: %+v)", fb.postCount, want, fb.postCalls)
	}
}

// readLock takes the advisory lock the way a competing hfd process would, so a
// test can prove the second invocation refuses to write.
func readLock(t *testing.T, home string) func() {
	t.Helper()
	dir := filepath.Join(home, ".config", "hfd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir lock dir: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := flockEx(f); err != nil {
		f.Close()
		t.Fatalf("flock: %v", err)
	}
	return func() { flockUn(f); f.Close() }
}
