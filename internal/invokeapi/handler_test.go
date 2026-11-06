// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package invokeapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/auth"
	"github.com/loch-sh/lambda-runtime/internal/engine"
	"github.com/loch-sh/lambda-runtime/internal/queue"
)

const testUUID = "6909e2ac-1cc9-4a2d-849b-b387a4a052c9"

type fixture struct {
	t     *testing.T
	q     *queue.Queue
	srv   *httptest.Server
	state engine.State
	// serve answers the popped jobs; nil leaves them queued.
	serve func(*engine.Job)
}

func newFixture(t *testing.T, opts Options, serve func(*engine.Job)) *fixture {
	f := &fixture{t: t, state: engine.StateReady, serve: serve}
	if opts.Queue == nil {
		opts.Queue = queue.New(4, 1<<20)
	}
	f.q = opts.Queue
	if opts.MaxPayload == 0 {
		opts.MaxPayload = 1024
	}
	if opts.QueueTimeout == 0 {
		opts.QueueTimeout = 5 * time.Second
	}
	opts.Function = FunctionName{Name: "resize", Region: "eu-west-3"}
	opts.Version = "$LATEST"
	opts.State = func() engine.State { return f.state }
	opts.Log = slog.New(slog.DiscardHandler)
	f.srv = httptest.NewServer(NewHandler(opts))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); f.srv.Close() })
	if serve != nil {
		go func() {
			for {
				job, err := f.q.Pop(ctx)
				if err != nil {
					return
				}
				serve(job)
			}
		}()
	}
	return f
}

func echo(job *engine.Job) {
	engine.Finish(job, &engine.Result{Kind: engine.KindSuccess, Payload: job.Payload, ContentType: "application/json",
		LogTail: []byte("START\nEND\n")})
}

// waitFor waits up to 5 s for cond.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
	}
}

// dialHeaderOnly sends the headers of a POST announcing a body of length
// bytes, and never sends the body.
func dialHeaderOnly(t *testing.T, f *fixture, path string, length int) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", path, length)
	return c
}

// readResponse reads the response on c, waiting at most d.
func readResponse(t *testing.T, c net.Conn, d time.Duration) (*http.Response, []byte, error) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return nil, nil, err
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, b, nil
}

func (f *fixture) do(method, path, body string, header http.Header) (*http.Response, []byte) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

const invokePath = "/2015-03-31/functions/function/invocations"

func wantAWSError(t *testing.T, resp *http.Response, body []byte, status int, name string) {
	t.Helper()
	var doc map[string]string
	if resp.StatusCode != status || resp.Header.Get("X-Amzn-ErrorType") != name ||
		resp.Header.Get("X-Amzn-RequestId") == "" || json.Unmarshal(body, &doc) != nil || doc["message"] == "" {
		t.Fatalf("got %d %s %s, want %d %s", resp.StatusCode, resp.Header.Get("X-Amzn-ErrorType"), body, status, name)
	}
}

func TestSuccess(t *testing.T) {
	f := newFixture(t, Options{}, echo)
	resp, body := f.do("POST", invokePath, `{"a":1}`, http.Header{
		"X-Amz-Log-Type":   {"Tail"},
		"X-Amzn-RequestId": {testUUID},
	})
	if resp.StatusCode != 200 || string(body) != `{"a":1}` {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	h := resp.Header
	tail, _ := base64.StdEncoding.DecodeString(h.Get("X-Amz-Log-Result"))
	if h.Get("X-Amzn-RequestId") == testUUID || len(h.Get("X-Amzn-RequestId")) != 36 || h.Get("X-Amz-Executed-Version") != "$LATEST" ||
		h.Get("X-Amz-Function-Error") != "" || string(tail) != "START\nEND\n" || h.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", h)
	}
	// Without Tail, no log result.
	resp, _ = f.do("POST", invokePath, `{}`, nil)
	if _, ok := resp.Header["X-Amz-Log-Result"]; ok {
		t.Error("X-Amz-Log-Result without Tail")
	}
}

func TestFunctionErrorAndEmptyBody(t *testing.T) {
	var got []byte
	f := newFixture(t, Options{}, func(job *engine.Job) {
		got = job.Payload
		engine.Finish(job, &engine.Result{Kind: engine.KindError, Payload: []byte(`{"errorType":"E"}`), ContentType: "application/json"})
	})
	resp, body := f.do("POST", invokePath, ``, nil)
	if resp.StatusCode != 200 || resp.Header.Get("X-Amz-Function-Error") != "Unhandled" || string(body) != `{"errorType":"E"}` {
		t.Fatalf("got %d %v %s", resp.StatusCode, resp.Header, body)
	}
	if len(got) != 0 {
		t.Errorf("empty body must be passed as is, got %q", got)
	}
}

func TestFunctionNames(t *testing.T) {
	f := newFixture(t, Options{}, echo)
	for _, name := range []string{
		"function", "resize", "resize:$LATEST", "function:%24LATEST",
		"arn:aws:lambda:eu-west-3:000000000000:function:resize",
		"arn:aws:lambda:eu-west-3:000000000000:function:resize:$LATEST",
		"000000000000:function:resize",
		"000000000000:function:resize:$LATEST",
	} {
		resp, body := f.do("POST", "/2015-03-31/functions/"+name+"/invocations", `{}`, nil)
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	for _, name := range []string{
		"other", "resize:1", "resize:prod",
		"arn:aws:lambda:us-east-1:000000000000:function:resize",
		"arn:aws:lambda::000000000000:function:resize",
		"arn:aws-cn:lambda:eu-west-3:000000000000:function:resize",
		"arn:aws:lambda:eu-west-3:123456789012:function:resize",
		"arn:aws:lambda:eu-west-3:000000000000:function:other",
		"000000000000:function:other",
		"123456789012:function:resize",
	} {
		resp, body := f.do("POST", "/2015-03-31/functions/"+name+"/invocations", `{}`, nil)
		wantAWSError(t, resp, body, 404, "ResourceNotFoundException")
	}
	resp, body := f.do("POST", invokePath+"?Qualifier=7", `{}`, nil)
	wantAWSError(t, resp, body, 404, "ResourceNotFoundException")
	if resp, _ := f.do("POST", invokePath+"?Qualifier=$LATEST", `{}`, nil); resp.StatusCode != 200 {
		t.Errorf("Qualifier=$LATEST: %d", resp.StatusCode)
	}
}

func TestRequestErrors(t *testing.T) {
	f := newFixture(t, Options{}, echo)
	cases := []struct {
		name, method, path, body string
		header                   http.Header
		status                   int
		errName                  string
	}{
		{"get", "GET", invokePath, "", nil, 405, "MethodNotAllowedException"},
		{"put", "PUT", invokePath, "", nil, 405, "MethodNotAllowedException"},
		{"unknown route", "POST", "/2015-03-31/functions", "", nil, 404, "ResourceNotFoundException"},
		{"event", "POST", invokePath, "{}", http.Header{"X-Amz-Invocation-Type": {"Event"}}, 400, "InvalidParameterValueException"},
		{"bad type", "POST", invokePath, "{}", http.Header{"X-Amz-Invocation-Type": {"Later"}}, 400, "InvalidParameterValueException"},
		{"client context not base64", "POST", invokePath, "{}", http.Header{"X-Amz-Client-Context": {"%%%"}}, 400, "InvalidRequestContentException"},
		{"client context not object", "POST", invokePath, "{}",
			http.Header{"X-Amz-Client-Context": {base64.StdEncoding.EncodeToString([]byte(`[1]`))}}, 400, "InvalidRequestContentException"},
		{"client context too long", "POST", invokePath, "{}",
			http.Header{"X-Amz-Client-Context": {strings.Repeat("A", 3584)}}, 400, "InvalidRequestContentException"},
		{"too large", "POST", invokePath, strings.Repeat("x", 1025), nil, 413, "RequestTooLargeException"},
	}
	for _, c := range cases {
		resp, body := f.do(c.method, c.path, c.body, c.header)
		wantAWSError(t, resp, body, c.status, c.errName)
		if c.status == 405 && resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s: Allow = %q", c.name, resp.Header.Get("Allow"))
		}
	}
	if s := f.q.Stats(); s.Depth != 0 || s.Bytes != 0 {
		t.Errorf("refused requests left room taken: %+v", s)
	}
}

func TestChunkedTooLarge(t *testing.T) {
	f := newFixture(t, Options{}, echo)
	req, _ := http.NewRequest("POST", f.srv.URL+invokePath, io.MultiReader(strings.NewReader(strings.Repeat("x", 2000))))
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	wantAWSError(t, resp, body, 413, "RequestTooLargeException")
	if s := f.q.Stats(); s.Depth != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestClientContextRelayedDecoded(t *testing.T) {
	var got string
	f := newFixture(t, Options{}, func(job *engine.Job) {
		got = job.ClientContext
		echo(job)
	})
	cc := `{"custom":{"k":"v"}}`
	f.do("POST", invokePath, `{}`, http.Header{"X-Amz-Client-Context": {base64.StdEncoding.EncodeToString([]byte(cc))}})
	if got != cc {
		t.Errorf("client context = %q", got)
	}
}

func TestTraceAndRequestIDs(t *testing.T) {
	var job *engine.Job
	f := newFixture(t, Options{}, func(j *engine.Job) { job = j; echo(j) })
	trace := "Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1"
	f.do("POST", invokePath, `{}`, http.Header{"X-Amzn-Trace-Id": {trace}, "X-Amzn-RequestId": {"not-a-uuid"}})
	if job.TraceID != trace || job.RequestID == "not-a-uuid" || len(job.RequestID) != 36 || job.Expires.IsZero() {
		t.Errorf("job = %+v", job.Request)
	}
	f.do("POST", invokePath, `{}`, http.Header{"X-Amzn-Trace-Id": {"Root=evil"}})
	if !strings.HasPrefix(job.TraceID, "Root=1-") || job.TraceID == "Root=evil" {
		t.Errorf("trace = %q", job.TraceID)
	}
}

func TestDryRun(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	resp, _ := f.do("POST", invokePath, `{}`, http.Header{"X-Amz-Invocation-Type": {"DryRun"}})
	if resp.StatusCode != 204 {
		t.Errorf("status %d", resp.StatusCode)
	}
	if s := f.q.Stats(); s.Depth != 0 || s.Bytes != 0 {
		t.Errorf("DryRun must not queue anything: %+v", s)
	}
}

func TestQueueFull(t *testing.T) {
	q := queue.New(1, 1<<20)
	f := newFixture(t, Options{Queue: q}, nil) // nobody pops
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.do("POST", invokePath, `{}`, nil)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for q.Stats().Depth == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	resp, body := f.do("POST", invokePath, `{}`, nil)
	wantAWSError(t, resp, body, 429, "TooManyRequestsException")
	var doc map[string]string
	_ = json.Unmarshal(body, &doc)
	if resp.Header.Get("Retry-After") != "1" || doc["Reason"] != "ReservedFunctionConcurrentInvocationLimitExceeded" {
		t.Errorf("429 = %v %s", resp.Header, body)
	}
	for _, j := range q.Close() {
		j.Fail(engine.ErrShuttingDown)
	}
	<-done
}

func TestQueueTimeout(t *testing.T) {
	f := newFixture(t, Options{QueueTimeout: 50 * time.Millisecond}, nil)
	resp, body := f.do("POST", invokePath, `{}`, nil)
	wantAWSError(t, resp, body, 429, "TooManyRequestsException")
	if s := f.q.Stats(); s.Depth != 0 {
		t.Errorf("timed-out job still queued: %+v", s)
	}
}

func TestShutdown(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	done := make(chan struct{})
	var resp *http.Response
	var body []byte
	go func() {
		defer close(done)
		resp, body = f.do("POST", invokePath, `{}`, nil)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.q.Stats().Depth == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for _, j := range f.q.Close() {
		j.Fail(engine.ErrShuttingDown)
	}
	<-done
	wantAWSError(t, resp, body, 503, "ServiceException")
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 without Retry-After")
	}
	resp, body = f.do("POST", invokePath, `{}`, nil)
	wantAWSError(t, resp, body, 503, "ServiceException")
}

func TestClientGoneWhileQueued(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", f.srv.URL+invokePath, strings.NewReader(`{}`))
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for f.q.Stats().Depth == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if _, err := f.srv.Client().Do(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.q.Stats().Depth != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("withdrawn job still queued: %+v", f.q.Stats())
		}
		time.Sleep(time.Millisecond)
	}
}

type denyAll struct{}

func (denyAll) Authenticate(*http.Request) (auth.BodyCheck, error) {
	return auth.BodyCheck{}, &auth.Error{Status: 401, Name: "MissingAuthenticationTokenException", Message: "no"}
}

func TestAuthenticatorRunsFirst(t *testing.T) {
	f := newFixture(t, Options{Auth: denyAll{}}, echo)
	resp, body := f.do("POST", invokePath, strings.Repeat("x", 5000), nil)
	wantAWSError(t, resp, body, 401, "MissingAuthenticationTokenException")
}

type brokenAuth struct{}

func (brokenAuth) Authenticate(*http.Request) (auth.BodyCheck, error) {
	return auth.BodyCheck{}, errors.New("bug")
}

func TestUnexpectedErrorIs500(t *testing.T) {
	f := newFixture(t, Options{Auth: brokenAuth{}}, echo)
	resp, body := f.do("POST", invokePath, `{}`, nil)
	wantAWSError(t, resp, body, 500, "ServiceException")
}

func TestHealth(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	resp, body := f.do("GET", "/_loch/health", "", nil)
	if resp.StatusCode != 200 || string(body) != `{"status":"ready","queueDepth":0}` {
		t.Errorf("health = %d %s", resp.StatusCode, body)
	}
	for _, s := range []engine.State{engine.StateInitializing, engine.StateError, engine.StateStopping} {
		f.state = s
		resp, body := f.do("GET", "/_loch/health", "", nil)
		if resp.StatusCode != 503 || !strings.Contains(string(body), string(s)) {
			t.Errorf("%s: %d %s", s, resp.StatusCode, body)
		}
	}
	if resp, _ := f.do("POST", "/_loch/health", "", nil); resp.StatusCode != 405 {
		t.Errorf("POST health = %d", resp.StatusCode)
	}
}

func TestNewServerLimits(t *testing.T) {
	srv := NewServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if srv.ReadHeaderTimeout != 5*time.Second || srv.IdleTimeout != 5*time.Second || srv.MaxHeaderBytes != 32<<10 || srv.WriteTimeout != 0 {
		t.Errorf("server = %+v", srv)
	}
}

func TestSlowBodyIsCut(t *testing.T) {
	f := newFixture(t, Options{}, echo)
	pr, pw := io.Pipe()
	req, _ := http.NewRequest("POST", f.srv.URL+invokePath, pr)
	req.ContentLength = 100
	go func() {
		_, _ = pw.Write([]byte("{"))
		time.Sleep(bodyProgress + time.Second) // stalls longer than allowed
		pw.Close()
	}()
	resp, err := f.srv.Client().Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatalf("stalled upload accepted")
		}
	}
	if s := f.q.Stats(); s.Depth != 0 || s.Bytes != 0 {
		t.Errorf("stalled upload kept its reservation: %+v", s)
	}
}

func TestChunkedBodyAccepted(t *testing.T) {
	var got []byte
	f := newFixture(t, Options{}, func(j *engine.Job) { got = j.Payload; echo(j) })
	req, _ := http.NewRequest("POST", f.srv.URL+invokePath, io.MultiReader(strings.NewReader(`{"a":`), strings.NewReader(`1}`)))
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != `{"a":1}` || cap(got) > 2*len(got) {
		t.Errorf("status %d, payload %q (cap %d)", resp.StatusCode, got, cap(got))
	}
}

func TestRelayedContentType(t *testing.T) {
	ct := ""
	f := newFixture(t, Options{}, func(j *engine.Job) {
		engine.Finish(j, &engine.Result{Kind: engine.KindSuccess, Payload: []byte("x"), ContentType: ct})
	})
	for in, want := range map[string]string{
		"application/json":                     "application/json",
		"text/plain; charset=utf-8":            "text/plain; charset=utf-8",
		"":                                     "application/json",
		"no slash":                             "application/json",
		"text/html" + strings.Repeat("x", 200): "application/json",
	} {
		ct = in
		resp, _ := f.do("POST", invokePath, `{}`, nil)
		if got := resp.Header.Get("Content-Type"); got != want || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%q: Content-Type = %q", in, got)
		}
	}
}

func TestTenantAndCompactClientContext(t *testing.T) {
	var job *engine.Job
	f := newFixture(t, Options{}, func(j *engine.Job) { job = j; echo(j) })
	cc := base64.StdEncoding.EncodeToString([]byte("{\n  \"custom\": {\"k\": \"v\"}\n}"))
	f.do("POST", invokePath, `{}`, http.Header{"X-Amz-Client-Context": {cc}, "X-Amz-Tenant-Id": {"tenant-1"}})
	if job.ClientContext != `{"custom":{"k":"v"}}` || job.TenantID != "tenant-1" {
		t.Errorf("job = %+v", job.Request)
	}
	f.do("POST", invokePath, `{}`, http.Header{"X-Amz-Tenant-Id": {"bad tenant!"}})
	if job.TenantID != "" {
		t.Errorf("invalid tenant relayed: %q", job.TenantID)
	}
}

func TestPoppedJobExpiredByEngine(t *testing.T) {
	// The engine expires a popped job at its Expires: the client gets 429.
	f := newFixture(t, Options{}, func(j *engine.Job) { j.Fail(engine.ErrQueueTimeout) })
	resp, body := f.do("POST", invokePath, `{}`, nil)
	wantAWSError(t, resp, body, 429, "TooManyRequestsException")
}

func TestThrottledResponseIsValidHTTP(t *testing.T) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(ThrottledResponse)), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	var doc map[string]string
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" || json.Unmarshal(body, &doc) != nil || doc["Type"] != "User" {
		t.Errorf("response = %d %v %s", resp.StatusCode, resp.Header, body)
	}
}

func TestInFlightPerIP(t *testing.T) {
	// A non-loopback caller is capped; the test server sees loopback, so
	// exercise the limiter directly, then its use in the handler.
	l := newIPLimiter(2)
	r1, ok1 := l.acquire("10.0.0.5:1")
	_, ok2 := l.acquire("10.0.0.5:2")
	_, ok3 := l.acquire("10.0.0.5:3")
	_, other := l.acquire("10.0.0.6:1")
	if !ok1 || !ok2 || ok3 || !other {
		t.Fatalf("acquire: %v %v %v %v", ok1, ok2, ok3, other)
	}
	r1()
	if _, ok := l.acquire("10.0.0.5:4"); !ok {
		t.Error("slot not released")
	}
	for _, addr := range []string{"127.0.0.1:5", "[::1]:5", "garbage"} {
		for range 5 {
			if _, ok := l.acquire(addr); !ok {
				t.Errorf("%s limited", addr)
			}
		}
	}
	var nilLimiter *ipLimiter
	if _, ok := nilLimiter.acquire("10.0.0.5:1"); !ok {
		t.Error("nil limiter limited")
	}
}

func TestInFlightPerIPInHandler(t *testing.T) {
	f := newFixture(t, Options{MaxInFlightPerIP: 1, RetryAfter: func() int { return 99 }}, nil)
	h := NewHandler(Options{MaxInFlightPerIP: 1, Queue: f.q, MaxPayload: 1024, QueueTimeout: time.Second,
		Function: FunctionName{Name: "f", Region: "r"}, State: func() engine.State { return engine.StateReady },
		Log: slog.New(slog.DiscardHandler), RetryAfter: func() int { return 99 }})
	release, _ := h.perIP.acquire("10.1.1.1:1") // the IP already has its request
	defer release()
	req := httptest.NewRequest("POST", invokePath, strings.NewReader(`{}`))
	req.RemoteAddr = "10.1.1.1:2"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 || rec.Header().Get("Connection") != "close" || rec.Header().Get("Retry-After") != "30" {
		t.Errorf("got %d %v", rec.Code, rec.Header())
	}
}

func TestChunkedAllocation(t *testing.T) {
	const max = 6 << 20
	body := strings.Repeat("x", max-10)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	data, err := readChunked(io.MultiReader(strings.NewReader(body)), max)
	runtime.ReadMemStats(&after)
	if err != nil || len(data) != len(body) {
		t.Fatalf("read: %d %v", len(data), err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > max+(1<<20) {
		t.Errorf("allocated %d bytes for a %d-byte body", allocated, len(body))
	}
	small, _ := readChunked(strings.NewReader("abc"), max)
	if string(small) != "abc" || cap(small) > 64 {
		t.Errorf("small body = %q (cap %d)", small, cap(small))
	}
	var tooLarge *http.MaxBytesError
	if _, err := readChunked(strings.NewReader("abcdef"), 5); !errors.As(err, &tooLarge) {
		t.Errorf("over max: %v", err)
	}
}

func TestEmptyTailOmitted(t *testing.T) {
	f := newFixture(t, Options{}, func(j *engine.Job) {
		engine.Finish(j, &engine.Result{Kind: engine.KindInitError, Payload: []byte(`{}`), ContentType: "application/json"})
	})
	resp, _ := f.do("POST", invokePath, `{}`, http.Header{"X-Amz-Log-Type": {"Tail"}})
	if _, ok := resp.Header["X-Amz-Log-Result"]; ok {
		t.Error("empty log tail sent")
	}
}

// bodyAuth authenticates the request with its body, like SigV4 without a
// payload hash header: it refuses bodies other than "good".
type bodyAuth struct{}

func (bodyAuth) Authenticate(*http.Request) (auth.BodyCheck, error) {
	return auth.BodyCheck{Verify: checkGood, Pending: true}, nil
}

func checkGood(body []byte) error {
	if string(body) != "good" {
		return &auth.Error{Status: 403, Name: "InvalidSignatureException", Message: "bad"}
	}
	return nil
}

// hashAuth authenticates the request at once and checks the integrity of
// its body, like SigV4 with a payload hash header.
type hashAuth struct{}

func (hashAuth) Authenticate(*http.Request) (auth.BodyCheck, error) {
	return auth.BodyCheck{Verify: checkGood}, nil
}

// modeAuth authenticates like SigV4 with a payload hash header when the
// request carries X-Hash, and like SigV4 without it otherwise.
type modeAuth struct{}

func (modeAuth) Authenticate(r *http.Request) (auth.BodyCheck, error) {
	if r.Header.Get("X-Hash") != "" {
		return hashAuth{}.Authenticate(r)
	}
	return bodyAuth{}.Authenticate(r)
}

func TestBodyCheck(t *testing.T) {
	f := newFixture(t, Options{Auth: bodyAuth{}}, echo)
	resp, body := f.do("POST", invokePath, "evil", nil)
	wantAWSError(t, resp, body, 403, "InvalidSignatureException")
	if s := f.q.Stats(); s.Depth != 0 || s.Bytes != 0 {
		t.Errorf("refused body kept its reservation: %+v", s)
	}
	if resp, _ := f.do("POST", invokePath, "good", nil); resp.StatusCode != 200 {
		t.Errorf("good body: %d", resp.StatusCode)
	}
}

type countObserver struct{ throttled, authFailed int }

func (c *countObserver) Throttled()  { c.throttled++ }
func (c *countObserver) AuthFailed() { c.authFailed++ }

type fixedMetrics string

func (m fixedMetrics) WriteTo(w io.Writer) (int64, error) {
	n, err := io.WriteString(w, string(m))
	return int64(n), err
}

func TestMetricsEndpoint(t *testing.T) {
	obs := &countObserver{}
	f := newFixture(t, Options{QueueTimeout: 10 * time.Millisecond, Observer: obs, Metrics: fixedMetrics("m 1\n")}, nil)
	resp, body := f.do("GET", "/_loch/metrics", "", nil)
	if resp.StatusCode != 200 || string(body) != "m 1\n" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("metrics = %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.do("POST", "/_loch/metrics", "", nil); resp.StatusCode != 405 {
		t.Errorf("POST metrics = %d", resp.StatusCode)
	}
	f.do("POST", invokePath, `{}`, nil) // queue timeout: throttled
	if obs.throttled != 1 {
		t.Errorf("throttled = %d", obs.throttled)
	}
	denied := newFixture(t, Options{Auth: denyAll{}, Metrics: fixedMetrics("x")}, nil)
	resp, body = denied.do("GET", "/_loch/metrics", "", nil)
	wantAWSError(t, resp, body, 401, "MissingAuthenticationTokenException")
	none := newFixture(t, Options{}, nil)
	resp, body = none.do("GET", "/_loch/metrics", "", nil)
	wantAWSError(t, resp, body, 404, "ResourceNotFoundException")
	// The body check of a SigV4 GET runs on an empty body.
	checked := newFixture(t, Options{Auth: bodyAuth{}, Metrics: fixedMetrics("x")}, nil)
	resp, body = checked.do("GET", "/_loch/metrics", "", nil)
	wantAWSError(t, resp, body, 403, "InvalidSignatureException")
}

func TestAuthFailureClosesAndCounts(t *testing.T) {
	obs := &countObserver{}
	f := newFixture(t, Options{Auth: bodyAuth{}, Observer: obs}, echo)
	resp, body := f.do("POST", invokePath, "evil", nil)
	wantAWSError(t, resp, body, 403, "InvalidSignatureException")
	if !resp.Close || obs.authFailed != 1 {
		t.Errorf("close=%v failures=%d", resp.Close, obs.authFailed)
	}
	denied := newFixture(t, Options{Auth: denyAll{}, Observer: obs}, echo)
	resp, _ = denied.do("POST", invokePath, "{}", nil)
	if !resp.Close || obs.authFailed != 2 {
		t.Errorf("close=%v failures=%d", resp.Close, obs.authFailed)
	}
}

func TestLogLimiter(t *testing.T) {
	l := newLogLimiter()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	if !l.allow("10.0.0.1:1") || l.allow("10.0.0.1:2") || !l.allow("10.0.0.2:1") {
		t.Fatal("first log per IP only")
	}
	now = now.Add(logEvery)
	if !l.allow("10.0.0.1:3") {
		t.Error("allowed again after a minute")
	}
	if l.allow("garbage") {
		t.Error("unparsable address logged")
	}
	// The map stays bounded.
	for i := range maxLoggedIPs + 10 {
		l.allow(netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)}).String() + ":1")
	}
	if len(l.last) > maxLoggedIPs {
		t.Errorf("limiter holds %d IPs", len(l.last))
	}
	if !l.allow("10.9.9.9:1") {
		t.Error("a new IP must still be logged when the table is full")
	}
}

func TestDryRunVerifiesTheBody(t *testing.T) {
	f := newFixture(t, Options{Auth: bodyAuth{}}, nil)
	resp, body := f.do("POST", invokePath, "evil", http.Header{"X-Amz-Invocation-Type": {"DryRun"}})
	wantAWSError(t, resp, body, 403, "InvalidSignatureException")
	resp, _ = f.do("POST", invokePath, "good", http.Header{"X-Amz-Invocation-Type": {"DryRun"}})
	if resp.StatusCode != 204 {
		t.Errorf("good DryRun: %d", resp.StatusCode)
	}
	if s := f.q.Stats(); s.Depth != 0 || s.Bytes != 0 {
		t.Errorf("DryRun kept room: %+v", s)
	}
}

func TestLoopbackWithoutAuth(t *testing.T) {
	// The test server is on the loopback: exempt when enabled.
	f := newFixture(t, Options{Auth: denyAll{}, LoopbackWithoutAuth: true, Metrics: fixedMetrics("m")}, echo)
	if resp, _ := f.do("POST", invokePath, `{}`, nil); resp.StatusCode != 200 {
		t.Errorf("loopback invoke: %d", resp.StatusCode)
	}
	if resp, _ := f.do("GET", "/_loch/metrics", "", nil); resp.StatusCode != 200 {
		t.Errorf("loopback metrics: %d", resp.StatusCode)
	}
	// A non-loopback caller still needs the credential.
	h := NewHandler(Options{Auth: denyAll{}, LoopbackWithoutAuth: true, Queue: f.q, MaxPayload: 1024,
		Function: FunctionName{Name: "f", Region: "r"}, State: func() engine.State { return engine.StateReady },
		Log: slog.New(slog.DiscardHandler)})
	req := httptest.NewRequest("POST", invokePath, strings.NewReader(`{}`))
	req.RemoteAddr = "10.0.0.9:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("remote caller: %d", rec.Code)
	}
	for addr, want := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "[::ffff:127.0.0.1]:1": true, "10.0.0.1:1": false, "x": false} {
		if isLoopback(addr) != want {
			t.Errorf("isLoopback(%s) != %v", addr, want)
		}
	}
}

func TestUnverifiedBodiesUseTheirOwnBudget(t *testing.T) {
	q := queue.New(10, 1<<20)
	q.SetUnverifiedLimits(queue.UnverifiedLimits{Bytes: 100, PerIP: 100, Count: 10, CountPerIP: 10})
	f := newFixture(t, Options{Auth: bodyAuth{}, Queue: q}, echo)
	// Larger than the unverified budget: refused before reading.
	resp, body := f.do("POST", invokePath, strings.Repeat("x", 200), nil)
	wantAWSError(t, resp, body, 429, "TooManyRequestsException")
	// Within it: accepted once verified, and the budget is given back.
	if resp, _ := f.do("POST", invokePath, "good", nil); resp.StatusCode != 200 {
		t.Errorf("good: %d", resp.StatusCode)
	}
	if got := q.UnverifiedBytes(); got != 0 {
		t.Errorf("unverified bytes left: %d", got)
	}
}

func TestRemoteIP(t *testing.T) {
	if got := remoteIP("[::ffff:10.0.0.1]:5"); got != "10.0.0.1" {
		t.Errorf("remoteIP = %s", got)
	}
	if got := remoteIP("garbage"); got != "garbage" {
		t.Errorf("remoteIP = %s", got)
	}
}

// TestUnverifiedBodiesNeverSent: requests that pass the checks done before
// the body (a valid access key id), then never send it, take no queue
// slot, are capped in number, and get 408 once their deadline passes.
func TestUnverifiedBodiesNeverSent(t *testing.T) {
	q := queue.New(2, 1<<20)
	q.SetUnverifiedLimits(queue.UnverifiedLimits{Bytes: 1 << 20, PerIP: 1 << 20, Count: 3, CountPerIP: 3})
	f := newFixture(t, Options{Auth: modeAuth{}, Queue: q}, echo)
	stalled := make([]net.Conn, 3)
	for i := range stalled {
		stalled[i] = dialHeaderOnly(t, f, invokePath, 1)
	}
	waitFor(t, func() bool { return q.UnverifiedCount() == 3 })
	if s := q.Stats(); s.Depth != 0 {
		t.Errorf("stalled requests hold queue slots: %+v", s)
	}
	// Beyond the count cap: refused at once.
	resp, body := f.do("POST", invokePath, "good", nil)
	wantAWSError(t, resp, body, 429, "TooManyRequestsException")
	// A caller authenticated before its body (payload hash header) is served.
	if resp, _ := f.do("POST", invokePath, "good", http.Header{"X-Hash": {"1"}}); resp.StatusCode != 200 {
		t.Errorf("authenticated caller: %d", resp.StatusCode)
	}
	// The stalled requests time out with 408, closed, and give their room
	// back.
	for _, c := range stalled {
		resp, b, err := readResponse(t, c, bodyGrace+3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		wantAWSError(t, resp, b, 408, "RequestTimeoutException")
		if !resp.Close {
			t.Error("408 keeps the connection open")
		}
	}
	waitFor(t, func() bool { return q.UnverifiedCount() == 0 && q.Stats().Bytes == 0 })
}

// TestVerifiedIntoAFullQueue: the slot is taken once the request is
// verified; a full queue at that moment answers 429 and frees the room.
func TestVerifiedIntoAFullQueue(t *testing.T) {
	q := queue.New(1, 1<<20)
	f := newFixture(t, Options{Auth: bodyAuth{}, Queue: q}, nil)
	held, err := q.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	resp, body := f.do("POST", invokePath, "good", nil)
	wantAWSError(t, resp, body, 429, "TooManyRequestsException")
	if s := q.Stats(); s.Depth != 1 || s.Bytes != 1 || q.UnverifiedCount() != 0 {
		t.Errorf("room kept: %+v", s)
	}
}

// TestStalledUnverifiedCannotStarveTheQueue reproduces an attack: 16 SigV4
// requests from one IP, with a valid access key id (it travels in clear)
// but no payload hash header, announce a 1-byte body and never send it.
// They used to take the 16 slots of the queue, and every legitimate caller
// got 429. With the limits of the runtime (a quarter of the queue size, a
// quarter of that per IP), one is held, the others are refused at once,
// and the queue stays free.
func TestStalledUnverifiedCannotStarveTheQueue(t *testing.T) {
	const queueSize = 16
	q := queue.New(queueSize, 32<<20)
	q.SetUnverifiedLimits(queue.UnverifiedLimits{Bytes: 8 << 20, PerIP: 6 << 20, Count: queueSize / 4, CountPerIP: 1})
	f := newFixture(t, Options{Auth: modeAuth{}, Queue: q, MaxInFlightPerIP: queueSize + 1}, echo)
	dialHeaderOnly(t, f, invokePath, 1) // held until the end of the test
	waitFor(t, func() bool { return q.UnverifiedCount() == 1 })
	for range queueSize - 1 {
		resp, b, err := readResponse(t, dialHeaderOnly(t, f, invokePath, 1), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		wantAWSError(t, resp, b, 429, "TooManyRequestsException")
	}
	if s := q.Stats(); s.Depth != 0 || q.UnverifiedCount() != 1 {
		t.Errorf("stalled requests hold room: %+v, %d unverified", s, q.UnverifiedCount())
	}
	for range queueSize {
		if resp, _ := f.do("POST", invokePath, "good", http.Header{"X-Hash": {"1"}}); resp.StatusCode != 200 {
			t.Fatalf("legitimate caller: %d", resp.StatusCode)
		}
	}
}

// TestEarlyErrorsDoNotWaitForTheBody: an error answered before the body is
// read (404, 413) comes at once and closes the connection; net/http would
// otherwise wait for the announced body first.
func TestEarlyErrorsDoNotWaitForTheBody(t *testing.T) {
	f := newFixture(t, Options{}, echo)
	denied := newFixture(t, Options{Auth: denyAll{}}, echo)
	for _, c := range []struct {
		f      *fixture
		path   string
		length int
		want   int
	}{
		{f, "/2015-03-31/functions/other/invocations", 10, 404},
		{f, invokePath, 1 << 20, 413},
		{denied, invokePath, 10, 401},
	} {
		resp, _, err := readResponse(t, dialHeaderOnly(t, c.f, c.path, c.length), 2*time.Second)
		if err != nil {
			t.Fatalf("%d: %v", c.want, err)
		}
		if resp.StatusCode != c.want || !resp.Close {
			t.Errorf("%d: got %d close=%v", c.want, resp.StatusCode, resp.Close)
		}
	}
}
