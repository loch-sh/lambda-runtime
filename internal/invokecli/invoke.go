// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package invokecli is the invoke subcommand: it reads an event on stdin,
// invokes the local runtime with the configured credential, and writes one
// bounded JSON envelope on stdout. Loch.sh Panel runs it with docker exec,
// so that it never joins the function's network.
package invokecli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/config"
	"github.com/loch-sh/lambda-runtime/internal/netutil"
)

// Exit codes.
const (
	// ExitDelivered: the runtime answered, whatever the function did.
	ExitDelivered = 0
	// ExitTransport: no answer (connection, timeout, oversized answer).
	ExitTransport = 1
	// ExitInput: the event or the configuration is unusable.
	ExitInput = 2
)

// errorAllowance is added to the response limit for the API errors.
const errorAllowance = 64 << 10

// localClient talks to the local runtime only: no proxy from the
// environment (it would receive the credential and the event), no
// redirect.
var localClient = &http.Client{
	Transport: &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Envelope is the output of the invoke subcommand. Payload and LogTail are
// base64: the envelope stays valid JSON whatever the function returned.
type Envelope struct {
	StatusCode    int    `json:"statusCode"`
	FunctionError string `json:"functionError"`
	RequestID     string `json:"requestId"`
	DurationMs    int64  `json:"durationMs"`
	Payload       string `json:"payload"`
	LogTail       string `json:"logTail"`
}

// stdinTimeout bounds the read of the event: a forgotten terminal must not
// hold the command forever.
const stdinTimeout = 10 * time.Second

// errCallerGone ends the call when stdin closes after the event
// (--watch-stdin).
var errCallerGone = errors.New("stdin closed: the caller is gone")

// Run executes the invoke subcommand and returns its exit code. It needs
// no credential: it calls the runtime on the loopback, which is exempt from
// authentication (only the function and docker exec processes are there).
// It always ends by its deadline (--timeout, by default function timeout +
// queue timeout + 5 s), and with --watch-stdin as soon as stdin closes.
func Run(cfg *config.Config, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx, stop := context.WithTimeout(ctx, cfg.Invoke.Timeout)
	defer stop()
	read := readEvent
	if cfg.Invoke.WatchStdin {
		read = readFirstJSON
	}
	event, rest, err := read(ctx, stdin, cfg.Runtime.MaxPayload)
	if err != nil {
		fmt.Fprintln(stderr, "invoke:", err)
		return ExitInput
	}
	if rest != nil {
		go func() {
			_, _ = io.Copy(io.Discard, rest) // returns when stdin closes
			cancel(errCallerGone)
		}()
	}
	env, err := invoke(ctx, cfg, event)
	if err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, errCallerGone) {
			err = cause
		}
		fmt.Fprintln(stderr, "invoke:", err)
		return ExitTransport
	}
	if err := json.NewEncoder(stdout).Encode(env); err != nil {
		fmt.Fprintln(stderr, "invoke:", err)
		return ExitTransport
	}
	return ExitDelivered
}

// readFunc reads the event; rest, when not nil, is the remainder of stdin
// to watch for the caller going away.
type readFunc func(ctx context.Context, stdin io.Reader, max int64) (event []byte, rest io.Reader, err error)

// readEvent reads the whole of stdin, at most max bytes, as the event.
func readEvent(ctx context.Context, stdin io.Reader, max int64) ([]byte, io.Reader, error) {
	event, err := withStdinTimeout(ctx, func() ([]byte, error) {
		b, err := io.ReadAll(io.LimitReader(stdin, max+1))
		if err == nil && int64(len(b)) > max {
			err = fmt.Errorf("the event must be at most %d bytes", max)
		}
		return b, err
	})
	return event, nil, err
}

// readFirstJSON reads the first JSON value of stdin as the event, leaving
// stdin open; the rest of stdin is returned to be watched. The event must
// be followed by a newline (or any whitespace): a number or a literal
// (42, true, null) only ends at the next byte, and stdin stays open.
// Objects, arrays and strings end by themselves; a pretty-printed,
// multi-line event works too.
func readFirstJSON(ctx context.Context, stdin io.Reader, max int64) ([]byte, io.Reader, error) {
	dec := json.NewDecoder(io.LimitReader(stdin, max+1))
	event, err := withStdinTimeout(ctx, func() ([]byte, error) {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("the event must be one JSON value of at most %d bytes: %w", max, err)
		}
		return raw, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return event, io.MultiReader(dec.Buffered(), stdin), nil
}

// withStdinTimeout runs read within stdinTimeout and the deadline of ctx.
func withStdinTimeout(ctx context.Context, read func() ([]byte, error)) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, stdinTimeout)
	defer cancel()
	type result struct {
		event []byte
		err   error
	}
	done := make(chan result, 1)
	go func() {
		b, err := read()
		done <- result{b, err}
	}()
	select {
	case r := <-done:
		return r.event, r.err
	case <-ctx.Done():
		return nil, errors.New("timed out reading the event on stdin (with --watch-stdin, end it with a newline)")
	}
}

func invoke(ctx context.Context, cfg *config.Config, event []byte) (*Envelope, error) {
	url := "http://" + netutil.LocalAddr(cfg.Runtime.InvokeAddr) + "/2015-03-31/functions/function/invocations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(event))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Invoke.LogTail {
		req.Header.Set("X-Amz-Log-Type", "Tail")
	}
	start := time.Now()
	resp, err := localClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	limit := cfg.Runtime.MaxResponse() + errorAllowance
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("the answer exceeds the response limit")
	}
	return &Envelope{
		StatusCode:    resp.StatusCode,
		FunctionError: resp.Header.Get("X-Amz-Function-Error"),
		RequestID:     resp.Header.Get("X-Amzn-RequestId"),
		DurationMs:    time.Since(start).Milliseconds(),
		Payload:       base64.StdEncoding.EncodeToString(body),
		LogTail:       resp.Header.Get("X-Amz-Log-Result"),
	}, nil
}
