// Package main — hfd, the Home Food Deli CLI.
//
// This file is the SHARED CONTRACT that client.go, commands.go and the tests all
// build against. It is locked per the signed-off plan (scratchpad/hfd-cli-plan-final.md,
// sections 2-6 + R3/R4 patches). Do not widen it without re-review.
package main

import (
	"io"
	"time"
)

// ---- Backend constants (public; anon key ships in the web bundle) ----
const (
	BaseURL = "https://isqknoojwebcomqrirog.supabase.co"
	// AnonKey is the public anon JWT (PostgREST reads only). NOT a secret.
	AnonKey = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJzdXBhYmFzZSIsInJlZiI6ImlzcWtub29qd2ViY29tcXJpcm9nIiwicm9sZSI6ImFub24iLCJpYXQiOjE3NzY4NDkyMTcsImV4cCI6MjA5MjQyNTIxN30.8UtUgpWbDxxYW4zdcEXA2StkuXdKB-ikFQGbzmmlkn4"

	// SAST is UTC+2, fixed. Do NOT use time.LoadLocation (fails in minimal static builds).
	sastOffsetSeconds = 2 * 60 * 60

	maxResponseBytes = 2 << 20 // 2 MiB response-body cap
	httpTimeout      = 20 * time.Second
	contractVersion  = 1
)

// SAST is the fixed +02:00 zone used for all "today"/weekday logic.
var SAST = time.FixedZone("SAST", sastOffsetSeconds)

// ---- Error codes (complete catalog; R4 patch #3) ----
type ErrCode string

const (
	CodeUsage          ErrCode = "USAGE"
	CodeValidation     ErrCode = "VALIDATION"
	CodeAuth           ErrCode = "AUTH"
	CodeRemote         ErrCode = "REMOTE"
	CodeOrderingClosed ErrCode = "ORDERING_CLOSED"
	CodeLocked         ErrCode = "LOCKED"
	CodeUnknownOutcome ErrCode = "UNKNOWN_OUTCOME"
)

// ---- JSON envelope (exactly one document per machine invocation) ----
type Envelope struct {
	Version int         `json:"version"`
	OK      bool        `json:"ok"`
	Data    interface{} `json:"data,omitempty"`
	Error   *ErrObject  `json:"error,omitempty"`
}

// ErrObject: status+details REQUIRED for REMOTE; details operation-discriminated for UNKNOWN_OUTCOME.
type ErrObject struct {
	Code      ErrCode     `json:"code"`
	Message   string      `json:"message"`
	Retryable bool        `json:"retryable"`
	Status    int         `json:"status,omitempty"`  // HTTP status for REMOTE
	Details   interface{} `json:"details,omitempty"` // e.g. UnknownOutcomeDetails
}

func okEnvelope(data interface{}) Envelope {
	return Envelope{Version: contractVersion, OK: true, Data: data}
}
func errEnvelope(e *ErrObject) Envelope {
	return Envelope{Version: contractVersion, OK: false, Error: e}
}

// CLIError is returned up to main by commands/client; main renders it as one Envelope + exit code.
type CLIError struct {
	Code      ErrCode
	Message   string
	Retryable bool
	Status    int
	Details   interface{}
}

func (e *CLIError) Error() string { return string(e.Code) + ": " + e.Message }
func (e *CLIError) toEnvelope() Envelope {
	return errEnvelope(&ErrObject{Code: e.Code, Message: e.Message, Retryable: e.Retryable, Status: e.Status, Details: e.Details})
}

// exitCode: 0 only on ok:true. Any CLIError → non-zero (2), USAGE → 64.
func (e *CLIError) exitCode() int {
	if e.Code == CodeUsage {
		return 64
	}
	return 2
}

// ---- Per-command data schemas (LOCKED; R4 patch #4 = order/orders fields except order_id
// are provisional/optional until live V1 confirms them). ----

type MenuData struct {
	MenuID      string     `json:"menu_id"`
	PublishedAt string     `json:"published_at"`
	DateBinding string     `json:"date_binding"` // always "weekday-only, not authoritative"
	Items       []MenuItem `json:"items"`
}
type MenuItem struct {
	ID        string `json:"id"`
	DayOfWeek string `json:"day_of_week"`
	Category  string `json:"category"`
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

type MenusData struct {
	Menus []MenuSummary `json:"menus"`
}
type MenuSummary struct {
	ID          string `json:"id"`
	Year        int    `json:"year"`
	Quarter     int    `json:"quarter"`
	QuarterWeek int    `json:"quarter_week"`
	PublishedAt string `json:"published_at"`
}

// OrderData: only OrderID is guaranteed until V1; the rest are best-effort (omitempty).
type OrderData struct {
	OrderID      string `json:"order_id"`
	Status       string `json:"status,omitempty"`
	MenuItemID   string `json:"menu_item_id,omitempty"`
	DeliveryDate string `json:"delivery_date,omitempty"`
	OrderName    string `json:"order_name,omitempty"`
	Self         bool   `json:"self"`
}

type OrdersData struct {
	Orders []OrderRecord `json:"orders"`
}
type OrderRecord struct {
	OrderID      string `json:"order_id"`
	Status       string `json:"status,omitempty"`
	MenuItemID   string `json:"menu_item_id,omitempty"`
	ItemName     string `json:"item_name,omitempty"`
	DeliveryDate string `json:"delivery_date,omitempty"`
	OrderName    string `json:"order_name,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
}

// CancelData.FinalState ∈ {"canceled","absent"} ONLY. Unresolved → UNKNOWN_OUTCOME error (R4 #2).
type CancelData struct {
	OrderID    string `json:"order_id"`
	FinalState string `json:"final_state"`
}

type NextData struct {
	Authoritative bool       `json:"authoritative"` // always false
	Dates         []NextDate `json:"dates"`
}
type NextDate struct {
	Date    string `json:"date"`    // YYYY-MM-DD
	Weekday string `json:"weekday"` // Monday..Friday
}

// Passthrough is the opaque result of call/get (explicitly not schema-stable).
type Passthrough struct {
	Passthrough bool        `json:"passthrough"` // always true
	HTTPStatus  int         `json:"http_status"`
	Body        interface{} `json:"body"`
}

// UnknownOutcomeDetails is operation-discriminated (R4 #1).
type UnknownOutcomeDetails struct {
	Operation          string       `json:"operation"`                     // "place-order" | "cancel-order"
	RequestFingerprint *Fingerprint `json:"request_fingerprint,omitempty"` // place-order
	OrderID            string       `json:"order_id,omitempty"`            // cancel-order
	Observations       interface{}  `json:"observations"`
}
type Fingerprint struct {
	MenuItemID   string `json:"menu_item_id"`
	DeliveryDate string `json:"delivery_date"`
	OrderName    string `json:"order_name,omitempty"`
}

// ---- Clock (injectable for deterministic tests) ----
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// ---- Client interface (implemented in client.go by Agent A) ----
//
// Auth routing (B5): edge-function calls carry the personal token; PostgREST reads carry the
// anon key ONLY. No redirects; body capped at maxResponseBytes; token never logged/returned.
type Backend interface {
	// EdgeGET / EdgePOST call /functions/v1/<fn> with the PERSONAL TOKEN.
	EdgeGET(fn string) (status int, body []byte, err error)
	EdgePOST(fn string, jsonBody []byte) (status int, body []byte, err error)
	// RestGET calls /rest/v1/<table>?<query> with the ANON KEY (never the token).
	RestGET(table, query string) (status int, body []byte, err error)
}

// Deps is the wiring bundle passed to every command handler.
type Deps struct {
	Backend Backend
	Clock   Clock
	Token   string    // personal hfd_ token; loaded once, never printed
	Stdin   io.Reader // for `call ... -`
}
