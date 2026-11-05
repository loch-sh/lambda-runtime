// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package runtimeapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testLimit = 1024

type server struct {
	*fixture
	srv *httptest.Server
}

func newServer(t *testing.T) *server {
	f := newFixture(t)
	srv := httptest.NewServer(NewHandler(f.m, testLimit, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return &server{fixture: f, srv: srv}
}

func (s *server) do(method, path string, body io.Reader, header http.Header) (*http.Response, []byte) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.srv.URL+path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := s.srv.Client().Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

type errorDoc struct {
	ErrorMessage string `json:"errorMessage"`
	ErrorType    string `json:"errorType"`
}

func wantAPIError(t *testing.T, resp *http.Response, body []byte, status int, errorType string) {
	t.Helper()
	var doc errorDoc
	if resp.StatusCode != status || json.Unmarshal(body, &doc) != nil || doc.ErrorType != errorType || doc.ErrorMessage == "" {
		t.Fatalf("got %d %s, want %d %s", resp.StatusCode, body, status, errorType)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
}

// invoke parks a /next over HTTP and delivers inv to it.
func (s *server) invoke(inv *Invocation) *http.Response {
	s.t.Helper()
	type result struct {
		resp *http.Response
		body []byte
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := s.srv.Client().Get(s.srv.URL + "/2018-06-01/runtime/invocation/next")
		if err != nil {
			ch <- result{}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		ch <- result{resp, b}
	}()
	if e := s.event(); e.Kind != EventIdle {
		s.t.Fatalf("event = %+v", e)
	}
	if err := s.deliver(inv); err != nil {
		s.t.Fatal(err)
	}
	r := <-ch
	if r.resp == nil || !bytes.Equal(r.body, inv.Payload) {
		s.t.Fatalf("next: %+v", r)
	}
	return r.resp
}

func TestNextHeaders(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	deadline := time.UnixMilli(1762430000123)
	resp := s.invoke(&Invocation{
		ID:              "6909e2ac-1cc9-4a2d-849b-b387a4a052c9",
		Payload:         []byte(`{"hello":"world"}`),
		Deadline:        deadline,
		FunctionARN:     "arn:aws:lambda:us-east-1:000000000000:function:f",
		TraceID:         "Root=1-x",
		ClientContext:   `{"custom":{}}`,
		CognitoIdentity: `{"cognitoIdentityId":"i"}`,
		TenantID:        "tenant-1",
	})
	want := map[string]string{
		"Lambda-Runtime-Aws-Request-Id":       "6909e2ac-1cc9-4a2d-849b-b387a4a052c9",
		"Lambda-Runtime-Deadline-Ms":          "1762430000123",
		"Lambda-Runtime-Invoked-Function-Arn": "arn:aws:lambda:us-east-1:000000000000:function:f",
		"Lambda-Runtime-Trace-Id":             "Root=1-x",
		"Lambda-Runtime-Client-Context":       `{"custom":{}}`,
		"Lambda-Runtime-Cognito-Identity":     `{"cognitoIdentityId":"i"}`,
		"Lambda-Runtime-Aws-Tenant-Id":        "tenant-1",
		"Content-Type":                        "application/json",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestNextOmitsEmptyOptionalHeaders(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	resp := s.invoke(&Invocation{ID: "a", Payload: []byte(`{}`)})
	for _, k := range []string{"Lambda-Runtime-Client-Context", "Lambda-Runtime-Cognito-Identity", "Lambda-Runtime-Aws-Tenant-Id"} {
		if _, ok := resp.Header[k]; ok {
			t.Errorf("%s must be absent", k)
		}
	}
}

func TestResponse(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	s.invoke(&Invocation{ID: "a/b", Payload: []byte(`{}`)})
	// The Node.js client percent-encodes the id.
	resp, body := s.do("POST", "/2018-06-01/runtime/invocation/"+url.PathEscape("a/b")+"/response",
		strings.NewReader(`{"ok":true}`), http.Header{"Content-Type": {"application/json"}})
	if resp.StatusCode != http.StatusAccepted || string(body) != `{"status":"OK"}` {
		t.Fatalf("response: %d %s", resp.StatusCode, body)
	}
	e := s.event()
	if e.Result.ID != "a/b" || string(e.Result.Body) != `{"ok":true}` || e.Result.ContentType != "application/json" {
		t.Errorf("result = %+v", e.Result)
	}
	// Second response for the same id.
	resp, body = s.do("POST", "/2018-06-01/runtime/invocation/a%2Fb/response", strings.NewReader(`1`), nil)
	wantAPIError(t, resp, body, http.StatusForbidden, "InvalidStateTransition")
	// Unknown id.
	resp, body = s.do("POST", "/2018-06-01/runtime/invocation/nope/response", strings.NewReader(`1`), nil)
	wantAPIError(t, resp, body, http.StatusBadRequest, "InvalidRequestID")
}

func TestInvocationError(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	s.invoke(&Invocation{ID: "a", Payload: []byte(`{}`)})
	doc := `{"errorType":"Error","errorMessage":"boom","trace":[]}`
	resp, _ := s.do("POST", "/2018-06-01/runtime/invocation/a/error", strings.NewReader(doc),
		http.Header{"Lambda-Runtime-Function-Error-Type": {"Error"}})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	e := s.event()
	if e.Result.Outcome != OutcomeFunctionError || e.Result.ErrorType != "Error" || string(e.Result.Body) != doc {
		t.Errorf("result = %+v", e.Result)
	}

	// No body, no type: a Lambda-shaped document is built.
	s.invoke(&Invocation{ID: "b", Payload: []byte(`{}`)})
	s.do("POST", "/2018-06-01/runtime/invocation/b/error", nil, nil)
	e = s.event()
	var got errorDoc
	if err := json.Unmarshal(e.Result.Body, &got); err != nil || got.ErrorType != "Runtime.Unknown" || e.Result.ErrorType != "Runtime.Unknown" {
		t.Errorf("synthesized error = %s (%+v)", e.Result.Body, e.Result)
	}

	// An absurdly long error type is bounded.
	s.invoke(&Invocation{ID: "c", Payload: []byte(`{}`)})
	s.do("POST", "/2018-06-01/runtime/invocation/c/error", nil,
		http.Header{"Lambda-Runtime-Function-Error-Type": {strings.Repeat("x", 1000)}})
	if e := s.event(); len(e.Result.ErrorType) != maxErrorTypeLen {
		t.Errorf("error type length = %d", len(e.Result.ErrorType))
	}
}

func TestResponseTooLarge(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	big := strings.Repeat("x", testLimit+1)

	// Known Content-Length: refused without reading.
	s.invoke(&Invocation{ID: "a", Payload: []byte(`{}`)})
	resp, body := s.do("POST", "/2018-06-01/runtime/invocation/a/response", strings.NewReader(big), nil)
	wantAPIError(t, resp, body, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge")
	if e := s.event(); e.Result.Outcome != OutcomeResponseTooLarge {
		t.Errorf("result = %+v", e.Result)
	}

	// Chunked: counted while reading.
	s.invoke(&Invocation{ID: "b", Payload: []byte(`{}`)})
	resp, body = s.do("POST", "/2018-06-01/runtime/invocation/b/response", io.MultiReader(strings.NewReader(big)), nil)
	wantAPIError(t, resp, body, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge")
	if e := s.event(); e.Result.Outcome != OutcomeResponseTooLarge || e.Result.ID != "b" {
		t.Errorf("result = %+v", e.Result)
	}

	// Exactly the limit is accepted.
	s.invoke(&Invocation{ID: "c", Payload: []byte(`{}`)})
	resp, _ = s.do("POST", "/2018-06-01/runtime/invocation/c/response", strings.NewReader(big[:testLimit]), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("limit-sized response: %d", resp.StatusCode)
	}
	if e := s.event(); e.Result.Outcome != OutcomeSuccess || len(e.Result.Body) != testLimit {
		t.Errorf("result = %+v", e.Result.Outcome)
	}

	// An oversized body for an unknown id is an unknown id.
	resp, body = s.do("POST", "/2018-06-01/runtime/invocation/zzz/response", strings.NewReader(big), nil)
	wantAPIError(t, resp, body, http.StatusBadRequest, "InvalidRequestID")
}

func TestStreamedResponse(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	post := func(id string, trailer http.Header) {
		req, _ := http.NewRequest("POST", s.srv.URL+"/2018-06-01/runtime/invocation/"+id+"/response",
			io.MultiReader(strings.NewReader("chunk1"), strings.NewReader("chunk2")))
		req.Header.Set("Lambda-Runtime-Function-Response-Mode", "streaming")
		req.Trailer = trailer
		resp, err := s.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	// A stream that ended well is a buffered success.
	s.invoke(&Invocation{ID: "a", Payload: []byte(`{}`)})
	post("a", nil)
	if e := s.event(); e.Result.Outcome != OutcomeSuccess || string(e.Result.Body) != "chunk1chunk2" {
		t.Errorf("result = %+v", e.Result)
	}
	// An error trailer turns it into a function error.
	s.invoke(&Invocation{ID: "b", Payload: []byte(`{}`)})
	doc := `{"errorType":"Boom","errorMessage":"mid-stream"}`
	post("b", http.Header{
		"Lambda-Runtime-Function-Error-Type": {"Boom"},
		"Lambda-Runtime-Function-Error-Body": {base64.StdEncoding.EncodeToString([]byte(doc))},
	})
	if e := s.event(); e.Result.Outcome != OutcomeFunctionError || e.Result.ErrorType != "Boom" || string(e.Result.Body) != doc {
		t.Errorf("result = %+v", e.Result)
	}
	// A broken error body trailer still yields an error document.
	s.invoke(&Invocation{ID: "c", Payload: []byte(`{}`)})
	post("c", http.Header{
		"Lambda-Runtime-Function-Error-Type": {"Boom"},
		"Lambda-Runtime-Function-Error-Body": {"%%%"},
	})
	if e := s.event(); e.Result.Outcome != OutcomeFunctionError || !strings.Contains(string(e.Result.Body), "Boom") {
		t.Errorf("result = %+v", e.Result)
	}
}

func TestInitErrorEndpoint(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	resp, _ := s.do("POST", "/2018-06-01/runtime/init/error",
		strings.NewReader(`{"errorType":"Runtime.ImportModuleError"}`),
		http.Header{"Lambda-Runtime-Function-Error-Type": {"Runtime.ImportModuleError"}})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	e := s.event()
	if e.Kind != EventInitError || e.InitError.ErrorType != "Runtime.ImportModuleError" {
		t.Errorf("event = %+v", e)
	}
	// Twice: the machine is closed.
	resp, body := s.do("POST", "/2018-06-01/runtime/init/error", nil, nil)
	wantAPIError(t, resp, body, http.StatusInternalServerError, "Runtime.Unavailable")

	// An oversized init error is still recorded, without its body.
	s.m.Reset()
	resp, _ = s.do("POST", "/2018-06-01/runtime/init/error", strings.NewReader(strings.Repeat("x", testLimit+1)), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if e := s.event(); e.InitError.ErrorType != "Runtime.Unknown" || !json.Valid(e.InitError.Body) {
		t.Errorf("event = %+v", e.InitError)
	}
}

func TestRoutingErrors(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	cases := []struct {
		method, path string
		status       int
		errorType    string
	}{
		{"POST", "/2018-06-01/runtime/invocation/next", 405, "MethodNotAllowed"},
		{"GET", "/2018-06-01/runtime/invocation/a/response", 405, "MethodNotAllowed"},
		{"PUT", "/2018-06-01/runtime/invocation/a/error", 405, "MethodNotAllowed"},
		{"GET", "/2018-06-01/runtime/init/error", 405, "MethodNotAllowed"},
		{"POST", "/2020-01-01/extension/register", 404, "NotFound"},
		{"PUT", "/2022-07-01/telemetry", 404, "NotFound"},
		{"GET", "/", 404, "NotFound"},
		{"GET", "/2018-06-01/runtime/restore/next", 404, "NotFound"},
		{"GET", "/2020-08-15/logs", 404, "NotFound"},
	}
	for _, c := range cases {
		resp, body := s.do(c.method, c.path, nil, nil)
		wantAPIError(t, resp, body, c.status, c.errorType)
		if c.status == 405 && resp.Header.Get("Allow") == "" {
			t.Errorf("%s %s: no Allow header", c.method, c.path)
		}
	}
	s.noEvent()
}

func TestConcurrentNextOverHTTP(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	go func() {
		resp, err := s.srv.Client().Get(s.srv.URL + "/2018-06-01/runtime/invocation/next")
		if err == nil {
			resp.Body.Close()
		}
	}()
	s.event()
	resp, body := s.do("GET", "/2018-06-01/runtime/invocation/next", nil, nil)
	wantAPIError(t, resp, body, http.StatusForbidden, "InvalidStateTransition")
	s.m.Close()
}

func TestErrorTypeResolution(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	cases := []struct {
		name, header, body, want string
	}{
		// Node and Python clients: no header, type in the body.
		{"node body", "", `{"errorType":"TypeError","errorMessage":"x","trace":[]}`, "TypeError"},
		{"python body", "", `{"errorMessage":"x","errorType":"ValueError","requestId":"r","stackTrace":[]}`, "ValueError"},
		{"header wins", "Custom.Type", `{"errorType":"Other"}`, "Custom.Type"},
		{"not json", "", `boom`, "Runtime.Unknown"},
		{"json without type", "", `{"errorMessage":"x"}`, "Runtime.Unknown"},
	}
	for i, c := range cases {
		id := string(rune('a' + i))
		s.invoke(&Invocation{ID: id, Payload: []byte(`{}`)})
		h := http.Header{}
		if c.header != "" {
			h.Set("Lambda-Runtime-Function-Error-Type", c.header)
		}
		s.do("POST", "/2018-06-01/runtime/invocation/"+id+"/error", strings.NewReader(c.body), h)
		e := s.event()
		if e.Result.ErrorType != c.want || string(e.Result.Body) != c.body {
			t.Errorf("%s: type = %q body = %s, want %q", c.name, e.Result.ErrorType, e.Result.Body, c.want)
		}
	}
}

func TestSnapStartMessage(t *testing.T) {
	s := newServer(t)
	_, body := s.do("GET", "/2018-06-01/runtime/restore/next", nil, nil)
	if !strings.Contains(string(body), "SnapStart is not enabled") {
		t.Errorf("body = %s", body)
	}
}

func TestNewServerHasNoTimeouts(t *testing.T) {
	srv := NewServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 || srv.IdleTimeout != 0 || srv.ReadHeaderTimeout != 0 {
		t.Errorf("timeouts must be unset: %+v", srv)
	}
	if srv.MaxHeaderBytes < 2<<20 {
		t.Errorf("MaxHeaderBytes = %d", srv.MaxHeaderBytes)
	}
}

func TestInitErrorAfterInitOverHTTP(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	s.invoke(&Invocation{ID: "a", Payload: []byte(`{}`)})
	resp, body := s.do("POST", "/2018-06-01/runtime/init/error", strings.NewReader(`{}`), nil)
	wantAPIError(t, resp, body, http.StatusForbidden, "InvalidStateTransition")
}

func TestSanitizeErrorType(t *testing.T) {
	cases := map[string]string{
		"Runtime.ImportModuleError": "Runtime.ImportModuleError",
		"Bad\r\nType\x00é":          "BadType",
		strings.Repeat("y", 300):    strings.Repeat("y", maxErrorTypeLen),
		"\x01\x02":                  "",
	}
	for in, want := range cases {
		if got := sanitizeErrorType(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := resolveErrorType("\x01", nil); got != "Runtime.Unknown" {
		t.Errorf("all-control type = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short"); got != "short" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate(strings.Repeat("z", 1000)); len(got) > maxLoggedLen+4 {
		t.Errorf("truncate length = %d", len(got))
	}
}

// deliver delivers to the current generation.
func (s *server) deliver(inv *Invocation) error { return s.fixture.deliver(inv) }

// TestNextDuringResponseUpload reproduces the Node.js client calling /next
// before the 202 of its streamed /response: the /next waits, and the owner
// sees the result before the idle event.
func TestNextDuringResponseUpload(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	s.invoke(&Invocation{ID: "a", Payload: []byte(`{}`)})
	pr, pw := io.Pipe()
	respStatus := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", s.srv.URL+"/2018-06-01/runtime/invocation/a/response", pr)
		resp, err := s.srv.Client().Do(req)
		if err != nil {
			respStatus <- -1
			return
		}
		resp.Body.Close()
		respStatus <- resp.StatusCode
	}()
	_, _ = pw.Write([]byte("partial"))
	time.Sleep(30 * time.Millisecond)
	nextStatus := make(chan int, 1)
	go func() {
		resp, err := s.srv.Client().Get(s.srv.URL + "/2018-06-01/runtime/invocation/next")
		if err != nil {
			nextStatus <- -1
			return
		}
		resp.Body.Close()
		nextStatus <- resp.StatusCode
	}()
	time.Sleep(30 * time.Millisecond)
	_, _ = pw.Write([]byte(" end"))
	pw.Close()
	if code := <-respStatus; code != http.StatusAccepted {
		t.Fatalf("/response = %d", code)
	}
	if e := s.event(); e.Kind != EventResult || string(e.Result.Body) != "partial end" {
		t.Fatalf("first event = %+v", e)
	}
	if e := s.event(); e.Kind != EventIdle {
		t.Fatalf("second event = %+v", e)
	}
	_ = s.deliver(&Invocation{ID: "b", Payload: []byte(`{}`)})
	if code := <-nextStatus; code != http.StatusOK {
		t.Errorf("/next = %d", code)
	}
}

func TestLongPollDisconnect(t *testing.T) {
	s := newServer(t)
	s.m.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", s.srv.URL+"/2018-06-01/runtime/invocation/next", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := s.srv.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	if e := s.event(); e.Kind != EventIdle || !e.Initial {
		t.Fatalf("event = %+v", e)
	}
	cancel()
	<-done
	// The server noticed: a new /next is accepted, still as the initial one.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.m.mu.Lock()
		p := s.m.phase
		s.m.mu.Unlock()
		if p == phaseInitializing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %v after the disconnect", p)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	func() {
		s := newServer(t)
		s.m.Reset()
		for i := range 20 {
			id := "id-" + strconv.Itoa(i)
			s.invoke(&Invocation{ID: id, Payload: []byte(`{}`)})
			s.do("POST", "/2018-06-01/runtime/invocation/"+id+"/response", strings.NewReader(`1`), nil)
			s.event()
		}
		s.m.Close()
		s.srv.Close()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+2 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
