package main

// STUB — Agent A implements httpBackend against the Backend interface in types.go.
func newHTTPBackend(token string) Backend { return &httpBackend{token: token} }

type httpBackend struct{ token string }

func (h *httpBackend) EdgeGET(fn string) (int, []byte, error)               { panic("unimplemented") }
func (h *httpBackend) EdgePOST(fn string, body []byte) (int, []byte, error) { panic("unimplemented") }
func (h *httpBackend) RestGET(table, query string) (int, []byte, error)     { panic("unimplemented") }
