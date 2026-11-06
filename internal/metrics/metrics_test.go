// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/engine"
)

func TestExposition(t *testing.T) {
	c := New("v1.0.0", func() (int, int) { return 2, 7 }, func() (int64, bool) { return 1 << 20, true })
	c.InitDone(150*time.Millisecond, true)
	c.InitDone(time.Second, false)
	c.StateChanged(engine.StateReady)
	c.InvocationDone(&engine.Result{Kind: engine.KindSuccess, Duration: 3 * time.Millisecond})
	c.InvocationDone(&engine.Result{Kind: engine.KindSuccess, Duration: 2 * time.Second})
	c.InvocationDone(&engine.Result{Kind: engine.KindTimeout, Duration: time.Hour})
	c.InvocationDone(&engine.Result{Kind: engine.KindInitError})
	c.Throttled()
	c.AuthFailed()

	var b bytes.Buffer
	n, err := c.WriteTo(&b)
	if err != nil || n != int64(b.Len()) {
		t.Fatalf("WriteTo = %d %v", n, err)
	}
	out := b.String()
	for _, want := range []string{
		`loch_lambda_runtime_info{version="v1.0.0"} 1`,
		`loch_lambda_invocations_total{outcome="success"} 2`,
		`loch_lambda_invocations_total{outcome="timeout"} 1`,
		`loch_lambda_invocations_total{outcome="init_error"} 1`,
		`loch_lambda_invocations_total{outcome="throttled"} 1`,
		`loch_lambda_invocations_total{outcome="oom"} 0`,
		`loch_lambda_invocation_duration_seconds_bucket{le="0.005"} 1`,
		`loch_lambda_invocation_duration_seconds_bucket{le="2.5"} 2`,
		`loch_lambda_invocation_duration_seconds_bucket{le="900"} 2`,
		`loch_lambda_invocation_duration_seconds_bucket{le="+Inf"} 3`,
		`loch_lambda_invocation_duration_seconds_count 3`,
		`loch_lambda_inits_total{result="success"} 1`,
		`loch_lambda_inits_total{result="failure"} 1`,
		`loch_lambda_init_duration_seconds_count 2`,
		`loch_lambda_auth_failures_total 1`,
		`loch_lambda_queue_depth 2`,
		`loch_lambda_queue_depth_max 7`,
		`loch_lambda_memory_peak_bytes 1048576`,
		`loch_lambda_state{state="ready"} 1`,
		`loch_lambda_state{state="error"} 0`,
		"# TYPE loch_lambda_invocation_duration_seconds histogram",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestNoMemoryWhenUnknown(t *testing.T) {
	c := New("dev", func() (int, int) { return 0, 0 }, func() (int64, bool) { return 0, false })
	var b bytes.Buffer
	_, _ = c.WriteTo(&b)
	if strings.Contains(b.String(), "memory_peak") {
		t.Error("memory peak exposed while unknown")
	}
	if !strings.Contains(b.String(), `loch_lambda_state{state="initializing"} 1`) {
		t.Error("initial state")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken") }

func TestWriteError(t *testing.T) {
	c := New("dev", func() (int, int) { return 0, 0 }, func() (int64, bool) { return 0, false })
	if _, err := c.WriteTo(failingWriter{}); err == nil {
		t.Error("write error not reported")
	}
}

// blockingWriter blocks until released.
type blockingWriter struct{ release chan struct{} }

func (b blockingWriter) Write(p []byte) (int, error) {
	<-b.release
	return len(p), nil
}

func TestSlowScraperDoesNotBlockUpdates(t *testing.T) {
	c := New("dev", func() (int, int) { return 0, 0 }, func() (int64, bool) { return 0, false })
	w := blockingWriter{release: make(chan struct{})}
	go func() { _, _ = c.WriteTo(w) }()
	time.Sleep(20 * time.Millisecond) // the scrape is blocked in Write
	done := make(chan struct{})
	go func() {
		c.InvocationDone(&engine.Result{Kind: engine.KindSuccess})
		c.StateChanged(engine.StateReady)
		c.Throttled()
		c.AuthFailed()
		c.InitDone(time.Millisecond, true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("updates blocked by a slow scrape")
	}
	close(w.release)
}
