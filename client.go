package main

// httpBackend is the only thing in the CLI that talks to the network.
//
// Transport safety (plan §4 + B5):
//   - fixed origin: every request is built field-wise from BaseURL; no caller-supplied URLs.
//   - path segments escaped (url.PathEscape), query re-encoded through url.Values.
//   - redirects refused, so the bearer token is never replayed to another host.
//   - hard total timeout, response body capped at maxResponseBytes.
//   - the personal token lives in the struct only: never logged, never in an error string.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

func newHTTPBackend(token string) Backend {
	return &httpBackend{
		token: token,
		hc: &http.Client{
			Timeout: httpTimeout,
			// Return the 3xx as-is instead of following it: a redirect must never
			// carry the Authorization header to a different origin.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type httpBackend struct {
	token string
	hc    *http.Client
}

// EdgeGET calls /functions/v1/<fn> with the personal token.
func (h *httpBackend) EdgeGET(fn string) (int, []byte, error) {
	return h.do(http.MethodGet, h.edgeURL(fn), nil, h.token)
}

// EdgePOST calls /functions/v1/<fn> with the personal token and a JSON body.
func (h *httpBackend) EdgePOST(fn string, jsonBody []byte) (int, []byte, error) {
	return h.do(http.MethodPost, h.edgeURL(fn), jsonBody, h.token)
}

// RestGET calls /rest/v1/<table>?<query> with the ANON key only — never the token.
func (h *httpBackend) RestGET(table, query string) (int, []byte, error) {
	u := BaseURL + "/rest/v1/" + url.PathEscape(table)
	if q := encodeQuery(query); q != "" {
		u += "?" + q
	}
	return h.do(http.MethodGet, u, nil, AnonKey)
}

func (h *httpBackend) edgeURL(fn string) string {
	return BaseURL + "/functions/v1/" + url.PathEscape(fn)
}

// encodeQuery normalises a caller-supplied query string. Anything unparseable is
// dropped rather than passed through raw, so no control characters reach the wire.
func encodeQuery(query string) string {
	if query == "" {
		return ""
	}
	v, err := url.ParseQuery(query)
	if err != nil {
		return ""
	}
	return v.Encode()
}

// do performs one request and returns the status plus the (possibly truncated) body.
// A non-2xx response is NOT an error — the command layer classifies it.
func (h *httpBackend) do(method, rawURL string, body []byte, bearer string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: could not build request")
	}
	req.Header.Set("apikey", AnonKey)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.hc.Do(req)
	if err != nil {
		// Strip the *url.Error wrapper: it repeats the URL, and we return only the cause.
		if ue, ok := err.(*url.Error); ok && ue.Err != nil {
			err = ue.Err
		}
		return 0, nil, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, b, fmt.Errorf("request failed: could not read response body")
	}
	return resp.StatusCode, b, nil
}
