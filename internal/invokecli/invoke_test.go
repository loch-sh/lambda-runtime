// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package invokecli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/config"
)

func testConfig(srv *httptest.Server) *config.Config {
	cfg := &config.Config{Command: config.CommandInvoke}
	cfg.Runtime.InvokeAddr = strings.TrimPrefix(srv.URL, "http://")
	cfg.Runtime.MaxPayload = 1024
	cfg.Invoke.Timeout = 5 * time.Second
	cfg.Function.Region = "eu-west-3"
	return cfg
}

func run(t *testing.T, cfg *config.Config, event string) (int, *Envelope, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(cfg, strings.NewReader(event), &out, &errOut)
	if code != ExitDelivered {
		return code, nil, errOut.String()
	}
	var env Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("envelope %q: %v", out.String(), err)
	}
	return code, &env, errOut.String()
}

func TestInvoke(t *testing.T) {
	var gotAuth, gotTail string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotTail = r.Header.Get("Authorization"), r.Header.Get("X-Amz-Log-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Amzn-RequestId", "req-1")
		w.Header().Set("X-Amz-Function-Error", "Unhandled")
		w.Header().Set("X-Amz-Log-Result", "U1RBUlQ=")
		_, _ = w.Write([]byte(`{"errorType":"E"}`))
	}))
	defer srv.Close()
	cfg := testConfig(srv)
	cfg.Runtime.Auth = config.Auth{Enabled: true, TokenSHA256: strings.Repeat("a", 64)}
	cfg.Invoke.LogTail = true
	code, env, stderr := run(t, cfg, `{"a":1}`)
	if code != ExitDelivered {
		t.Fatalf("code %d: %s", code, stderr)
	}
	payload, _ := base64.StdEncoding.DecodeString(env.Payload)
	if env.StatusCode != 200 || env.FunctionError != "Unhandled" || env.RequestID != "req-1" || env.LogTail != "U1RBUlQ=" ||
		string(payload) != `{"errorType":"E"}` || env.DurationMs < 0 {
		t.Errorf("envelope = %+v", env)
	}
	// No credential: the loopback is exempt.
	if gotAuth != "" || gotTail != "Tail" || string(gotBody) != `{"a":1}` {
		t.Errorf("request: auth %q tail %q body %q", gotAuth, gotTail, gotBody)
	}
}

// blockingReader never returns.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

func TestStdinTimeout(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runtime.MaxPayload = 10
	cfg.Invoke.Timeout = 50 * time.Millisecond
	var errOut bytes.Buffer
	if code := Run(cfg, blockingReader{}, io.Discard, &errOut); code != ExitInput || !strings.Contains(errOut.String(), "timed out") {
		t.Errorf("code %d: %s", code, errOut.String())
	}
}

func TestAPIErrorIsDelivered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	code, env, _ := run(t, testConfig(srv), `{}`)
	if code != ExitDelivered || env.StatusCode != 429 || env.FunctionError != "" {
		t.Errorf("code %d envelope %+v", code, env)
	}
}

func TestInputAndTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1024+100+errorAllowance+1))
	}))
	cfg := testConfig(srv)
	if code, _, stderr := run(t, cfg, strings.Repeat("x", 1025)); code != ExitInput || !strings.Contains(stderr, "at most 1024") {
		t.Errorf("oversized event: %d %s", code, stderr)
	}
	if code, _, stderr := run(t, cfg, `{}`); code != ExitTransport || !strings.Contains(stderr, "exceeds") {
		t.Errorf("oversized answer: %d %s", code, stderr)
	}
	srv.Close()
	if code, _, _ := run(t, cfg, `{}`); code != ExitTransport {
		t.Errorf("unreachable: %d", code)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { time.Sleep(time.Second) }))
	defer slow.Close()
	cfg = testConfig(slow)
	cfg.Invoke.Timeout = 50 * time.Millisecond
	if code, _, _ := run(t, cfg, `{}`); code != ExitTransport {
		t.Errorf("timeout: %d", code)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestStdoutError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	if code := Run(testConfig(srv), strings.NewReader(`{}`), failWriter{}, io.Discard); code != ExitTransport {
		t.Errorf("code = %d", code)
	}
}

func TestNoProxyNoRedirect(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/", http.StatusFound)
	}))
	defer srv.Close()
	code, env, stderr := run(t, testConfig(srv), `{}`)
	if code != ExitDelivered || env.StatusCode != http.StatusFound {
		t.Errorf("code %d envelope %+v %s", code, env, stderr)
	}
}

func TestWatchStdin(t *testing.T) {
	var gotBody []byte
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()
	cfg := testConfig(srv)
	cfg.Invoke.WatchStdin = true

	// The event is the first JSON value; stdin stays open: delivered.
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte(`{"a": 1}  `)) }()
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	var out bytes.Buffer
	if code := Run(cfg, pr, &out, io.Discard); code != ExitDelivered || string(gotBody) != `{"a": 1}` {
		t.Fatalf("code %d body %q", code, gotBody)
	}
	pw.Close()

	// stdin closes while the invocation runs: the command ends at once.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	cfg = testConfig(slow)
	cfg.Invoke.WatchStdin = true
	pr, pw = io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"b":2}`))
		time.Sleep(200 * time.Millisecond)
		pw.Close()
	}()
	start := time.Now()
	var errOut bytes.Buffer
	code := Run(cfg, pr, io.Discard, &errOut)
	if code != ExitTransport || !strings.Contains(errOut.String(), "caller is gone") || time.Since(start) > time.Second {
		t.Errorf("caller gone: code %d after %s: %s", code, time.Since(start), errOut.String())
	}

	// Not JSON, or closed before a whole value: input error.
	for _, in := range []string{"nope", `{"a":`, ""} {
		if code := Run(cfg, strings.NewReader(in), io.Discard, io.Discard); code != ExitInput {
			t.Errorf("%q: code %d", in, code)
		}
	}
}

// TestWatchStdinEveryJSONType: with --watch-stdin, each kind of JSON value,
// ended by a newline, is read at once while stdin stays open; a number or
// a literal cannot end before that newline.
func TestWatchStdinEveryJSONType(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()
	cfg := testConfig(srv)
	cfg.Invoke.WatchStdin = true
	for _, event := range []string{`{"a":[1,{"b":null}]}`, `[1,"two",3]`, `42`, `-1.5e3`, `"hi"`, `true`, `false`, `null`} {
		pr, pw := io.Pipe()
		go func() { _, _ = pw.Write([]byte(event + "\n")) }() // stdin stays open
		start := time.Now()
		code := Run(cfg, pr, io.Discard, io.Discard)
		if code != ExitDelivered || got != event || time.Since(start) > time.Second {
			t.Errorf("%s: code %d, sent %q, after %s", event, code, got, time.Since(start))
		}
		pw.Close()
	}
}
