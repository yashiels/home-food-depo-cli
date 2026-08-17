package main

// contract_test.go — the agent-facing contract that is not a write: the JSON
// envelope, exit codes, the self-describing help catalog, `next` date math,
// the `menu` weekday filter, `orders`, and the passthrough commands.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---- 8. envelope + exit codes ---------------------------------------------

func TestErrorEnvelopeAndExitCodes(t *testing.T) {
	cases := []struct {
		name     string
		err      *CLIError
		wantExit int
	}{
		{"usage", &CLIError{Code: CodeUsage, Message: "unknown command: nope"}, 64},
		{"validation", &CLIError{Code: CodeValidation, Message: "bad uuid"}, 2},
		{"auth", &CLIError{Code: CodeAuth, Message: "no token"}, 2},
		{"remote", &CLIError{Code: CodeRemote, Message: "status 500", Retryable: true, Status: 500}, 2},
		{"ordering closed", &CLIError{Code: CodeOrderingClosed, Message: "closed", Status: 400}, 2},
		{"locked", &CLIError{Code: CodeLocked, Message: "in progress", Retryable: true}, 2},
		{"unknown outcome", &CLIError{Code: CodeUnknownOutcome, Message: "sent, unconfirmed"}, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.err.toEnvelope()
			if env.OK {
				t.Fatalf("error envelope has ok:true")
			}
			if env.Version != 1 {
				t.Fatalf("version = %d, want 1", env.Version)
			}
			if env.Data != nil {
				t.Fatalf("error envelope carries data: %#v", env.Data)
			}
			if env.Error == nil {
				t.Fatalf("error envelope has no error object")
			}
			if env.Error.Code != tc.err.Code {
				t.Fatalf("code = %s, want %s", env.Error.Code, tc.err.Code)
			}
			if env.Error.Message != tc.err.Message {
				t.Fatalf("message = %q", env.Error.Message)
			}
			if env.Error.Retryable != tc.err.Retryable {
				t.Fatalf("retryable = %v", env.Error.Retryable)
			}
			if got := tc.err.exitCode(); got != tc.wantExit {
				t.Fatalf("exitCode = %d, want %d", got, tc.wantExit)
			}
			if tc.err.Error() != string(tc.err.Code)+": "+tc.err.Message {
				t.Fatalf("Error() = %q", tc.err.Error())
			}

			// The rendered document must be one valid JSON object with ok:false.
			var m map[string]interface{}
			if err := json.Unmarshal(mustJSON(env), &m); err != nil {
				t.Fatalf("envelope is not valid JSON: %v", err)
			}
			if m["ok"] != false {
				t.Fatalf("rendered ok = %v, want false", m["ok"])
			}
			if _, has := m["data"]; has {
				t.Fatalf("rendered error envelope has a data key")
			}
		})
	}
}

func TestOKEnvelope(t *testing.T) {
	env := okEnvelope(&NextData{Authoritative: false, Dates: []NextDate{{Date: "2026-03-16", Weekday: "Monday"}}})
	if !env.OK {
		t.Fatalf("ok envelope has ok:false")
	}
	if env.Version != 1 {
		t.Fatalf("version = %d, want 1", env.Version)
	}
	if contractVersion != 1 {
		t.Fatalf("contractVersion = %d, want 1", contractVersion)
	}
	if env.Error != nil {
		t.Fatalf("ok envelope carries an error object")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(mustJSON(env), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if m["ok"] != true {
		t.Fatalf("rendered ok = %v", m["ok"])
	}
	if _, has := m["error"]; has {
		t.Fatalf("ok envelope rendered an error key")
	}
	if _, has := m["data"]; !has {
		t.Fatalf("ok envelope rendered no data key")
	}
}

// REMOTE-class errors must carry status + details (B4).
func TestRemoteErrorCarriesStatusAndDetails(t *testing.T) {
	fb := &fakeBackend{restGetFunc: func(string, string) (int, []byte, error) {
		return 500, []byte(`{"message":"boom"}`), nil
	}}
	data, err := cmdMenus(newTestDeps(fb, time.Time{}, ""), nil)
	cerr := requireCLIError(t, data, err, CodeRemote)

	if cerr.Status != 500 {
		t.Fatalf("status = %d, want 500", cerr.Status)
	}
	if !cerr.Retryable {
		t.Fatalf("a 5xx read should be retryable")
	}
	det, ok := cerr.Details.(map[string]interface{})
	if !ok || det["remote_body"] == nil {
		t.Fatalf("details missing remote_body: %#v", cerr.Details)
	}
}

// ---- 9. help --json --------------------------------------------------------

func TestHelpJSONIsValidAndComplete(t *testing.T) {
	raw, err := json.Marshal(helpJSON())
	if err != nil {
		t.Fatalf("help --json does not marshal: %v", err)
	}
	var cat struct {
		Version    int      `json:"version"`
		BaseURL    string   `json:"base_url"`
		Functions  []string `json:"functions"`
		ErrorCodes []string `json:"error_codes"`
		Envelope   struct {
			Description string `json:"description"`
			Success     string `json:"success"`
			Error       string `json:"error"`
			ExitCodes   string `json:"exit_codes"`
		} `json:"envelope"`
		Commands []struct {
			Name        string   `json:"name"`
			Args        []string `json:"args"`
			Flags       []string `json:"flags"`
			Description string   `json:"description"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatalf("help --json is not valid JSON: %v", err)
	}

	if cat.Version != 1 {
		t.Fatalf("catalog version = %d, want 1", cat.Version)
	}
	if cat.BaseURL != BaseURL {
		t.Fatalf("base_url = %q", cat.BaseURL)
	}
	if len(cat.Commands) != 9 {
		t.Fatalf("catalog lists %d commands, want 9", len(cat.Commands))
	}
	if len(cat.ErrorCodes) != 7 {
		t.Fatalf("catalog lists %d error codes, want 7", len(cat.ErrorCodes))
	}

	wantCodes := map[string]bool{
		"USAGE": true, "VALIDATION": true, "AUTH": true, "REMOTE": true,
		"ORDERING_CLOSED": true, "LOCKED": true, "UNKNOWN_OUTCOME": true,
	}
	for _, c := range cat.ErrorCodes {
		if !wantCodes[c] {
			t.Fatalf("unexpected error code %q in the catalog", c)
		}
		delete(wantCodes, c)
	}
	if len(wantCodes) != 0 {
		t.Fatalf("catalog is missing error codes: %v", wantCodes)
	}

	wantCmds := map[string]bool{
		"menu": true, "menus": true, "order": true, "orders": true, "cancel": true,
		"call": true, "get": true, "next": true, "help": true,
	}
	for _, c := range cat.Commands {
		if !wantCmds[c.Name] {
			t.Fatalf("unexpected command %q in the catalog", c.Name)
		}
		delete(wantCmds, c.Name)
		if c.Description == "" {
			t.Fatalf("command %q has no description", c.Name)
		}
	}
	if len(wantCmds) != 0 {
		t.Fatalf("catalog is missing commands: %v", wantCmds)
	}

	if cat.Envelope.Success == "" || cat.Envelope.Error == "" || cat.Envelope.ExitCodes == "" {
		t.Fatalf("envelope schema is incomplete: %+v", cat.Envelope)
	}
	if len(cat.Functions) != 3 {
		t.Fatalf("functions = %v, want the 3 edge functions", cat.Functions)
	}
}

// Every catalog command except `help` must have a dispatchable handler, and
// every handler must be documented. (help is handled in main before dispatch.)
func TestHelpCatalogMatchesHandlers(t *testing.T) {
	documented := map[string]bool{}
	for _, c := range helpCommands() {
		documented[c.Name] = true
	}
	for name := range handlers {
		if !documented[name] {
			t.Fatalf("handler %q is not in the help catalog", name)
		}
	}
	for name := range documented {
		if name == "help" {
			continue
		}
		if _, ok := handlers[name]; !ok {
			t.Fatalf("catalog documents %q but there is no handler", name)
		}
	}
	if len(handlers) != 8 {
		t.Fatalf("%d handlers, want 8 (9 commands minus help)", len(handlers))
	}
}

func TestHelpTextMentionsEveryCommandAndCode(t *testing.T) {
	txt := helpText()
	for _, c := range helpCommands() {
		if !strings.Contains(txt, c.Name) {
			t.Fatalf("help text does not mention %q", c.Name)
		}
	}
	for _, code := range []ErrCode{CodeUsage, CodeValidation, CodeAuth, CodeRemote, CodeOrderingClosed, CodeLocked, CodeUnknownOutcome} {
		if !strings.Contains(txt, string(code)) {
			t.Fatalf("help text does not mention %s", code)
		}
	}
	if strings.Contains(cmdHelpText("order"), "unknown command") {
		t.Fatalf("per-command help for order not found")
	}
	if !strings.Contains(cmdHelpText("definitely-not-a-command"), "unknown command") {
		t.Fatalf("unknown per-command help should say so")
	}
}

// ---- 10. next --------------------------------------------------------------

func TestNextReturnsFollowingWeekMonToFri(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want []string
	}{
		{
			name: "on a Monday",
			now:  time.Date(2026, 3, 9, 10, 0, 0, 0, SAST),
			want: []string{"2026-03-16", "2026-03-17", "2026-03-18", "2026-03-19", "2026-03-20"},
		},
		{
			name: "on the Friday of the same week",
			now:  time.Date(2026, 3, 13, 8, 59, 0, 0, SAST),
			want: []string{"2026-03-16", "2026-03-17", "2026-03-18", "2026-03-19", "2026-03-20"},
		},
		{
			name: "on the Sunday that closes that week",
			now:  time.Date(2026, 3, 15, 23, 30, 0, 0, SAST),
			want: []string{"2026-03-16", "2026-03-17", "2026-03-18", "2026-03-19", "2026-03-20"},
		},
		{
			name: "across a month boundary",
			now:  time.Date(2026, 3, 30, 12, 0, 0, 0, SAST),
			want: []string{"2026-04-06", "2026-04-07", "2026-04-08", "2026-04-09", "2026-04-10"},
		},
		{
			name: "UTC instant is read in SAST, not UTC",
			// 2026-03-15T23:00Z is already Monday 2026-03-16 01:00 in SAST.
			now:  time.Date(2026, 3, 15, 23, 0, 0, 0, time.UTC),
			want: []string{"2026-03-23", "2026-03-24", "2026-03-25", "2026-03-26", "2026-03-27"},
		},
	}

	weekdays := []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday"}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBackend{}
			data, err := cmdNext(newTestDeps(fb, tc.now, ""), nil)
			requireNoError(t, err)

			nd, ok := data.(*NextData)
			if !ok {
				t.Fatalf("data is %T, want *NextData", data)
			}
			if nd.Authoritative {
				t.Fatalf("next must always be authoritative:false")
			}
			if len(nd.Dates) != 5 {
				t.Fatalf("got %d dates, want 5", len(nd.Dates))
			}
			for i, d := range nd.Dates {
				if d.Date != tc.want[i] {
					t.Fatalf("date[%d] = %q, want %q", i, d.Date, tc.want[i])
				}
				if d.Weekday != weekdays[i] {
					t.Fatalf("weekday[%d] = %q, want %q", i, d.Weekday, weekdays[i])
				}
				if _, ok := parseDate(d.Date); !ok {
					t.Fatalf("date[%d] = %q is not strict YYYY-MM-DD", i, d.Date)
				}
			}
			if fb.getCount+fb.postCount+fb.rstCount != 0 {
				t.Fatalf("next must be pure local date math, but hit the backend")
			}
		})
	}
}

func TestNextRejectsArguments(t *testing.T) {
	fb := &fakeBackend{}
	data, err := cmdNext(newTestDeps(fb, time.Date(2026, 3, 9, 0, 0, 0, 0, SAST), ""), []string{"2026-03-16"})
	requireCLIError(t, data, err, CodeUsage)
}

// ---- 11. menu weekday filter ----------------------------------------------

// menuFixture serves two published menus and items spread across weekdays.
func menuFixture() restGetFn {
	menus := []interface{}{
		map[string]interface{}{
			"id": menuUUID, "year": 2026, "quarter": 1, "quarter_week": 11,
			"published_at": "2026-03-06T09:00:00Z",
		},
		map[string]interface{}{
			"id": otherUUID, "year": 2026, "quarter": 1, "quarter_week": 10,
			"published_at": "2026-02-27T09:00:00Z",
		},
	}
	items := []interface{}{
		map[string]interface{}{"id": itemUUID, "day_of_week": "Monday", "category": "Main", "name": "Bobotie", "sort_order": 1},
		map[string]interface{}{"id": altOrderID, "day_of_week": "Mon", "category": "Salad", "name": "Greek Salad", "sort_order": 2},
		map[string]interface{}{"id": newOrderID, "day_of_week": "Tuesday", "category": "Main", "name": "Lasagne", "sort_order": 3},
		map[string]interface{}{"id": otherUUID, "day_of_week": "FRIDAY", "category": "Main", "name": "Fish", "sort_order": 4},
	}
	return func(table, query string) (int, []byte, error) {
		switch table {
		case "menus":
			return 200, mustJSON(menus), nil
		case "menu_items":
			return 200, mustJSON(items), nil
		}
		return 404, []byte(`{"message":"no such table"}`), nil
	}
}

func TestMenuDateFiltersByWeekdayOnly(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantNames []string
	}{
		{"no date returns every item", nil, []string{"Bobotie", "Greek Salad", "Lasagne", "Fish"}},
		{"Monday date", []string{"2026-03-16"}, []string{"Bobotie", "Greek Salad"}},
		{"Tuesday date", []string{"2026-03-17"}, []string{"Lasagne"}},
		{"Friday date, case-insensitive", []string{"2026-03-20"}, []string{"Fish"}},
		{"Wednesday date matches nothing", []string{"2026-03-18"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBackend{restGetFunc: menuFixture()}
			data, err := cmdMenu(newTestDeps(fb, time.Time{}, ""), tc.args)
			requireNoError(t, err)

			md, ok := data.(*MenuData)
			if !ok {
				t.Fatalf("data is %T, want *MenuData", data)
			}
			if md.MenuID != menuUUID {
				t.Fatalf("menu_id = %q, want the newest published menu %q", md.MenuID, menuUUID)
			}
			if md.PublishedAt != "2026-03-06T09:00:00Z" {
				t.Fatalf("published_at = %q", md.PublishedAt)
			}
			if md.DateBinding != dateBindingLabel {
				t.Fatalf("date_binding = %q, want %q", md.DateBinding, dateBindingLabel)
			}
			if len(md.Items) != len(tc.wantNames) {
				t.Fatalf("got %d items %+v, want %d", len(md.Items), md.Items, len(tc.wantNames))
			}
			for i, want := range tc.wantNames {
				if md.Items[i].Name != want {
					t.Fatalf("item[%d] = %q, want %q", i, md.Items[i].Name, want)
				}
			}
			// The date must never leave the machine: menus carry no dates.
			for _, c := range fb.restCalls {
				if strings.Contains(c.Query, "2026-03") {
					t.Fatalf("the date argument leaked into a backend query: %s", c.Query)
				}
			}
			// Reads are anon-key PostgREST reads, never edge-function calls.
			if fb.getCount+fb.postCount != 0 {
				t.Fatalf("menu must not call any edge function")
			}
		})
	}
}

func TestMenuArgumentValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want ErrCode
	}{
		{"bad date", []string{"2026-13-01"}, CodeValidation},
		{"impossible date", []string{"2026-02-30"}, CodeValidation},
		{"bad menu id", []string{"--menu-id", "nope"}, CodeValidation},
		{"two positionals", []string{"2026-03-16", "2026-03-17"}, CodeUsage},
		{"unknown flag", []string{"--menu", menuUUID}, CodeUsage},
		{"flag without a value", []string{"--menu-id"}, CodeUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBackend{restGetFunc: menuFixture()}
			data, err := cmdMenu(newTestDeps(fb, time.Time{}, ""), tc.args)
			requireCLIError(t, data, err, tc.want)
			if fb.rstCount != 0 {
				t.Fatalf("rejected menu args still hit the backend")
			}
		})
	}
}

func TestMenuByIDAndEmptyResult(t *testing.T) {
	fb := &fakeBackend{restGetFunc: menuFixture()}
	data, err := cmdMenu(newTestDeps(fb, time.Time{}, ""), []string{"--menu-id=" + menuUUID})
	requireNoError(t, err)
	if md := data.(*MenuData); md.MenuID != menuUUID {
		t.Fatalf("menu_id = %q", md.MenuID)
	}
	if !strings.Contains(fb.restCalls[0].Query, "id=eq."+menuUUID) {
		t.Fatalf("--menu-id was not pushed into the query: %s", fb.restCalls[0].Query)
	}

	empty := &fakeBackend{restGetFunc: func(string, string) (int, []byte, error) {
		return 200, []byte(`[]`), nil
	}}
	data, cerr := cmdMenu(newTestDeps(empty, time.Time{}, ""), nil)
	requireCLIError(t, data, cerr, CodeRemote)
}

func TestMenusListsPublishedMenus(t *testing.T) {
	fb := &fakeBackend{restGetFunc: menuFixture()}
	data, err := cmdMenus(newTestDeps(fb, time.Time{}, ""), nil)
	requireNoError(t, err)

	ms, ok := data.(*MenusData)
	if !ok {
		t.Fatalf("data is %T, want *MenusData", data)
	}
	if len(ms.Menus) != 2 {
		t.Fatalf("got %d menus, want 2", len(ms.Menus))
	}
	if ms.Menus[0].ID != menuUUID || ms.Menus[0].Year != 2026 || ms.Menus[0].QuarterWeek != 11 {
		t.Fatalf("first menu = %+v", ms.Menus[0])
	}

	data, cerr := cmdMenus(newTestDeps(fb, time.Time{}, ""), []string{"extra"})
	requireCLIError(t, data, cerr, CodeUsage)
}

// ---- orders ----------------------------------------------------------------

func TestOrdersDecodesBothBackendShapes(t *testing.T) {
	rows := []map[string]interface{}{
		{
			"id": newOrderID, "state": "confirmed", "date": deliverDate,
			"menu_items": map[string]interface{}{"id": itemUUID, "name": "Bobotie"},
			"createdAt":  "2026-03-13T07:00:00Z",
		},
	}

	cases := []struct {
		name string
		body []byte
	}{
		{"wrapped in {orders:[...]}", ordersBody(rows[0])},
		{"bare array", mustJSON([]interface{}{rows[0]})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBackend{edgeGetFunc: func(fn string) (int, []byte, error) {
				if fn != "my-orders" {
					t.Fatalf("orders called %q, want my-orders", fn)
				}
				return 200, tc.body, nil
			}}
			data, err := cmdOrders(newTestDeps(fb, time.Time{}, ""), nil)
			requireNoError(t, err)

			od, ok := data.(*OrdersData)
			if !ok {
				t.Fatalf("data is %T, want *OrdersData", data)
			}
			if len(od.Orders) != 1 {
				t.Fatalf("got %d orders, want 1", len(od.Orders))
			}
			r := od.Orders[0]
			if r.OrderID != newOrderID || r.Status != "confirmed" || r.DeliveryDate != deliverDate {
				t.Fatalf("record = %+v", r)
			}
			if r.ItemName != "Bobotie" || r.MenuItemID != itemUUID {
				t.Fatalf("nested menu_items join not decoded: %+v", r)
			}
			if r.CreatedAt != "2026-03-13T07:00:00Z" {
				t.Fatalf("created_at = %q", r.CreatedAt)
			}
		})
	}
}

func TestOrdersRejectsArgsAndSurfacesAuthFailure(t *testing.T) {
	fb := &fakeBackend{}
	data, err := cmdOrders(newTestDeps(fb, time.Time{}, ""), []string{"all"})
	requireCLIError(t, data, err, CodeUsage)
	if fb.getCount != 0 {
		t.Fatalf("rejected orders args still hit the backend")
	}

	auth := &fakeBackend{edgeGetFunc: func(string) (int, []byte, error) {
		return 403, []byte(`{"message":"forbidden"}`), nil
	}}
	data, cerr := cmdOrders(newTestDeps(auth, time.Time{}, ""), nil)
	requireCLIError(t, data, cerr, CodeAuth)
}

// ---- passthrough: call / get ----------------------------------------------

func TestCallPassthrough(t *testing.T) {
	t.Run("GET returns status and body as data", func(t *testing.T) {
		fb := &fakeBackend{edgeGetFunc: func(fn string) (int, []byte, error) {
			if fn != "my-orders" {
				t.Fatalf("fn = %q", fn)
			}
			return 404, []byte(`{"message":"nope"}`), nil
		}}
		data, err := cmdCall(newTestDeps(fb, time.Time{}, ""), []string{"my-orders"})
		requireNoError(t, err) // a non-2xx is DATA here, not an error

		p, ok := data.(*Passthrough)
		if !ok {
			t.Fatalf("data is %T, want *Passthrough", data)
		}
		if !p.Passthrough || p.HTTPStatus != 404 {
			t.Fatalf("passthrough = %+v", p)
		}
	})

	t.Run("POST body from stdin keeps secrets out of argv", func(t *testing.T) {
		fb := &fakeBackend{edgePostFunc: statusPost(200, []byte(`{"ok":true}`))}
		d := newTestDeps(fb, time.Time{}, `{"order_id":"`+newOrderID+`"}`)

		data, err := cmdCall(d, []string{"--method", "POST", "cancel-order", "-"})
		requireNoError(t, err)
		requirePostCount(t, fb, 1)

		if got := string(fb.postCalls[0].Body); !strings.Contains(got, newOrderID) {
			t.Fatalf("stdin body was not forwarded: %q", got)
		}
		if p := data.(*Passthrough); p.HTTPStatus != 200 {
			t.Fatalf("http_status = %d", p.HTTPStatus)
		}
	})

	t.Run("POST with no body defaults to {}", func(t *testing.T) {
		fb := &fakeBackend{edgePostFunc: statusPost(200, []byte(`{}`))}
		_, err := cmdCall(newTestDeps(fb, time.Time{}, ""), []string{"--method=POST", "my-orders"})
		requireNoError(t, err)
		if string(fb.postCalls[0].Body) != "{}" {
			t.Fatalf("default body = %q", fb.postCalls[0].Body)
		}
	})

	t.Run("bypasses all validation", func(t *testing.T) {
		fb := &fakeBackend{}
		_, err := cmdCall(newTestDeps(fb, time.Time{}, ""), []string{"--method", "post", "any-function-name", `{"anything":1}`})
		requireNoError(t, err)
		requirePostCount(t, fb, 1)
	})

	usage := []struct {
		name string
		args []string
	}{
		{"no function", nil},
		{"bad method", []string{"--method", "DELETE", "my-orders"}},
		{"body with GET", []string{"my-orders", `{"a":1}`}},
		{"too many positionals", []string{"a", "b", "c"}},
	}
	for _, tc := range usage {
		t.Run("usage: "+tc.name, func(t *testing.T) {
			fb := &fakeBackend{}
			data, err := cmdCall(newTestDeps(fb, time.Time{}, ""), tc.args)
			requireCLIError(t, data, err, CodeUsage)
			requirePostCount(t, fb, 0)
		})
	}
}

func TestGetPassthrough(t *testing.T) {
	fb := &fakeBackend{restGetFunc: func(table, query string) (int, []byte, error) {
		if table != "companies" || query != "select=id,name" {
			t.Fatalf("get forwarded table=%q query=%q", table, query)
		}
		return 200, []byte(`[{"id":1}]`), nil
	}}
	data, err := cmdGet(newTestDeps(fb, time.Time{}, ""), []string{"companies", "select=id,name"})
	requireNoError(t, err)

	p, ok := data.(*Passthrough)
	if !ok {
		t.Fatalf("data is %T, want *Passthrough", data)
	}
	if !p.Passthrough || p.HTTPStatus != 200 {
		t.Fatalf("passthrough = %+v", p)
	}

	// Non-JSON bodies are surfaced verbatim rather than failing the command.
	raw := &fakeBackend{restGetFunc: func(string, string) (int, []byte, error) {
		return 503, []byte("service unavailable"), nil
	}}
	data, err = cmdGet(newTestDeps(raw, time.Time{}, ""), []string{"menus"})
	requireNoError(t, err)
	if body := data.(*Passthrough).Body; body != "service unavailable" {
		t.Fatalf("body = %#v", body)
	}

	// Transport failures are REMOTE + retryable.
	dead := &fakeBackend{restGetFunc: func(string, string) (int, []byte, error) {
		return 0, nil, errNoNetwork
	}}
	data, cerr := cmdGet(newTestDeps(dead, time.Time{}, ""), []string{"menus"})
	requireCLIError(t, data, cerr, CodeRemote)
	if !cerr.Retryable {
		t.Fatalf("transport failure should be retryable")
	}

	bad := &fakeBackend{}
	data, cerr = cmdGet(newTestDeps(bad, time.Time{}, ""), nil)
	requireCLIError(t, data, cerr, CodeUsage)
}

// ---- untrusted backend text is bounded ------------------------------------

func TestBackendTextIsTruncatedInDetails(t *testing.T) {
	long := strings.Repeat("A", 4096)
	fb := &fakeBackend{restGetFunc: func(string, string) (int, []byte, error) {
		return 500, []byte(long), nil
	}}
	data, err := cmdMenus(newTestDeps(fb, time.Time{}, ""), nil)
	cerr := requireCLIError(t, data, err, CodeRemote)

	det := cerr.Details.(map[string]interface{})
	body, _ := det["remote_body"].(string)
	if len(body) >= len(long) {
		t.Fatalf("remote_body was not truncated (%d bytes)", len(body))
	}
	if !strings.HasSuffix(body, "…(truncated)") {
		t.Fatalf("truncation is not labeled: %q", body[len(body)-20:])
	}
}
