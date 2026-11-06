// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/cgroup"
	"github.com/loch-sh/lambda-runtime/internal/ids"
	"github.com/loch-sh/lambda-runtime/internal/logs"
	"github.com/loch-sh/lambda-runtime/internal/process"
	"github.com/loch-sh/lambda-runtime/internal/runtimeapi"
)

func TestMain(m *testing.M) {
	if os.Getenv("FAKE_RUNTIME") == "1" {
		fakeRuntime()
		return
	}
	os.Exit(m.Run())
}

// chanSource is a Source fed by the tests.
type chanSource chan *Job

func (s chanSource) Pop(ctx context.Context) (*Job, error) {
	select {
	case j := <-s:
		return j, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// recorder is an Observer that keeps what it is told.
type recorder struct {
	mu      sync.Mutex
	inits   []bool
	results []Kind
	states  []State
}

func (r *recorder) InitDone(_ time.Duration, ok bool) {
	r.mu.Lock()
	r.inits = append(r.inits, ok)
	r.mu.Unlock()
}
func (r *recorder) InvocationDone(res *Result) {
	r.mu.Lock()
	r.results = append(r.results, res.Kind)
	r.mu.Unlock()
}
func (r *recorder) StateChanged(s State) { r.mu.Lock(); r.states = append(r.states, s); r.mu.Unlock() }

type harness struct {
	t      *testing.T
	engine *Engine
	source chanSource
	obs    *recorder
	cancel context.CancelFunc
	done   chan struct{}
	memDir string
}

type harnessOptions struct {
	env         []string
	timeout     time.Duration
	initTimeout time.Duration
	grace       time.Duration
}

func newHarness(t *testing.T, o harnessOptions) *harness {
	t.Helper()
	if o.timeout == 0 {
		o.timeout = 5 * time.Second
	}
	if o.initTimeout == 0 {
		o.initTimeout = 5 * time.Second
	}
	if o.grace == 0 {
		o.grace = 2 * time.Second
	}
	logger := slog.New(slog.DiscardHandler)
	events := make(chan runtimeapi.Event, 16)
	machine := runtimeapi.NewMachine(events)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := runtimeapi.NewServer(runtimeapi.NewHandler(machine, 1024, logger), logger)
	go srv.Serve(ln)

	reaper := process.NewReaper(logger)
	memDir := t.TempDir()
	h := &harness{t: t, source: make(chanSource), obs: &recorder{}, done: make(chan struct{}), memDir: memDir}
	env := append([]string{
		"FAKE_RUNTIME=1",
		"AWS_LAMBDA_RUNTIME_API=" + ln.Addr().String(),
		"FAKE_MEMORY_EVENTS=" + filepath.Join(memDir, "memory.events"),
	}, o.env...)
	if err := os.WriteFile(filepath.Join(memDir, "memory.events"), []byte("oom 0\noom_kill 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.engine = New(Options{
		Command:       []string{os.Args[0]},
		Env:           env,
		FunctionARN:   "arn:aws:lambda:us-east-1:000000000000:function:f",
		Timeout:       o.timeout,
		InitTimeout:   o.initTimeout,
		ShutdownGrace: o.grace,
		MaxResponse:   1024,
		Platform:      logs.Platform{Version: "$LATEST", MemoryMB: 128},
		Memory:        cgroup.NewMemory(memDir),
		Log:           logger,
		Observer:      h.obs,
	}, Dependencies{
		Machine: machine,
		Events:  events,
		Reaper:  reaper,
		Relay:   logs.NewRelay(io.Discard, io.Discard),
		Source:  h.source,
	}, os.Getpid())
	h.engine.testHook = make(chan func(*loop))

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		h.engine.Run(ctx)
	}()
	t.Cleanup(func() {
		h.stop()
		srv.Close()
		reaper.Close()
	})
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Error("engine did not stop")
	}
}

// submit queues an event and returns its job.
func (h *harness) submit(event string) *Job {
	h.t.Helper()
	job := NewJob(Request{RequestID: ids.NewRequestID(), Payload: []byte(event), TraceID: ids.NewTraceID(time.Now())})
	select {
	case h.source <- job:
	case <-time.After(10 * time.Second):
		h.t.Fatal("job not taken")
	}
	return job
}

func (h *harness) invoke(event string) *Result {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := h.submit(event).Wait(ctx)
	if err != nil {
		h.t.Fatalf("invoke %s: %v", event, err)
	}
	return res
}

func (h *harness) waitState(want State) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.engine.State() != want {
		if time.Now().After(deadline) {
			h.t.Fatalf("state = %s, want %s", h.engine.State(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func errorTypeOf(t *testing.T, r *Result) string {
	t.Helper()
	var doc struct{ ErrorType, ErrorMessage string }
	if err := json.Unmarshal(r.Payload, &doc); err != nil {
		t.Fatalf("payload %s: %v", r.Payload, err)
	}
	return doc.ErrorType
}

func TestSuccessAndLogs(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.waitState(StateReady)
	res := h.invoke(`{"op":"print"}`)
	if res.Kind != KindSuccess || string(res.Payload) != `"printed"` || res.FunctionError() || res.ContentType != "application/json" {
		t.Fatalf("result = %+v", res)
	}
	tail := string(res.LogTail)
	for _, want := range []string{"START RequestId: " + res.RequestID, "hello from the handler", "warning from the handler",
		"END RequestId: " + res.RequestID, "REPORT RequestId: " + res.RequestID, "Init Duration:"} {
		if !strings.Contains(tail, want) {
			t.Errorf("tail lacks %q:\n%s", want, tail)
		}
	}
	// Init duration only on the first report.
	res = h.invoke(`{"op":"echo"}`)
	if res.Kind != KindSuccess || strings.Contains(string(res.LogTail), "Init Duration") {
		t.Errorf("second result = %+v\n%s", res, res.LogTail)
	}
}

func TestFunctionError(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	res := h.invoke(`{"op":"fail"}`)
	if res.Kind != KindError || res.ErrorType != "MyError" || !res.FunctionError() || errorTypeOf(t, res) != "MyError" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(string(res.LogTail), "Status: error\tError Type: MyError") {
		t.Errorf("tail = %s", res.LogTail)
	}
	if h.invoke(`{"op":"echo"}`).Kind != KindSuccess {
		t.Error("runtime must keep serving after a function error")
	}
}

func TestTimeoutThenReinit(t *testing.T) {
	h := newHarness(t, harnessOptions{timeout: 300 * time.Millisecond})
	res := h.invoke(`{"op":"sleep","ms":5000}`)
	if res.Kind != KindTimeout || errorTypeOf(t, res) != "Sandbox.Timedout" ||
		!strings.Contains(string(res.Payload), "Task timed out after 0.30 seconds") {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
	if !strings.Contains(string(res.LogTail), "Status: timeout") || !strings.Contains(string(res.LogTail), "Task timed out") {
		t.Errorf("tail = %s", res.LogTail)
	}
	if res.Duration < 300*time.Millisecond || res.Duration > 3*time.Second {
		t.Errorf("duration = %s", res.Duration)
	}
	// The bootstrap was killed and re-inited.
	if res := h.invoke(`{"op":"echo"}`); res.Kind != KindSuccess {
		t.Errorf("after timeout = %+v", res)
	}
}

func TestExitDuringInvocation(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	res := h.invoke(`{"op":"exit"}`)
	if res.Kind != KindExit || errorTypeOf(t, res) != "Runtime.ExitError" ||
		!strings.Contains(string(res.Payload), "RequestId: "+res.RequestID+" Error: Runtime exited with error: exit status 3") {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
	if !strings.Contains(string(res.LogTail), "bye") {
		t.Errorf("tail = %s", res.LogTail)
	}
	if res := h.invoke(`{"op":"echo"}`); res.Kind != KindSuccess {
		t.Errorf("after exit = %+v", res)
	}
}

func TestOOMKill(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	res := h.invoke(`{"op":"oom"}`)
	if res.Kind != KindOOM || errorTypeOf(t, res) != "Runtime.OutOfMemory" ||
		!strings.Contains(string(res.Payload), "signal: killed") {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
	if res := h.invoke(`{"op":"echo"}`); res.Kind != KindSuccess {
		t.Errorf("after OOM = %+v", res)
	}
}

func TestResponseTooLarge(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	res := h.invoke(`{"op":"big","n":2000}`)
	if res.Kind != KindTooLarge || errorTypeOf(t, res) != "Function.ResponseSizeTooLarge" ||
		!strings.Contains(string(res.Payload), "(1024 bytes)") {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
	if res := h.invoke(`{"op":"echo"}`); res.Kind != KindSuccess {
		t.Errorf("after too large = %+v", res)
	}
}

func TestResponseThenExitKeepsTheResponse(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	res := h.invoke(`{"op":"respond-then-exit"}`)
	if res.Kind != KindSuccess || string(res.Payload) != `"done"` {
		t.Fatalf("result = %+v", res)
	}
	// Exit while idle: re-init, not an error.
	if res := h.invoke(`{"op":"echo"}`); res.Kind != KindSuccess {
		t.Errorf("after idle exit = %+v", res)
	}
	h.waitState(StateReady)
}

func TestInitErrorPosted(t *testing.T) {
	h := newHarness(t, harnessOptions{env: []string{"FAKE_INIT=error"}})
	h.waitState(StateError)
	res := h.invoke(`{}`)
	if res.Kind != KindInitError || res.ErrorType != "Runtime.ImportModuleError" ||
		errorTypeOf(t, res) != "Runtime.ImportModuleError" {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
}

func TestInitExit(t *testing.T) {
	h := newHarness(t, harnessOptions{env: []string{"FAKE_INIT=exit"}})
	h.waitState(StateError)
	res := h.invoke(`{}`)
	if res.Kind != KindInitError || errorTypeOf(t, res) != "Runtime.ExitError" ||
		!strings.Contains(string(res.Payload), "exit status 2") {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
	h.obs.mu.Lock()
	defer h.obs.mu.Unlock()
	if len(h.obs.inits) == 0 || h.obs.inits[0] {
		t.Errorf("inits = %v", h.obs.inits)
	}
}

func TestSlowInitIsChargedToFirstInvocation(t *testing.T) {
	h := newHarness(t, harnessOptions{
		env:         []string{"FAKE_INIT=slow:400"},
		initTimeout: 100 * time.Millisecond,
		timeout:     3 * time.Second,
	})
	res := h.invoke(`{"op":"echo"}`)
	if res.Kind != KindSuccess {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
	h.waitState(StateReady)
}

func TestSlowInitBeyondTimeout(t *testing.T) {
	h := newHarness(t, harnessOptions{
		env:         []string{"FAKE_INIT=slow:3000"},
		initTimeout: 50 * time.Millisecond,
		timeout:     300 * time.Millisecond,
	})
	time.Sleep(100 * time.Millisecond) // past the init timeout
	res := h.invoke(`{"op":"echo"}`)
	if res.Kind != KindTimeout {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
}

func TestQueueWaitsDuringInit(t *testing.T) {
	h := newHarness(t, harnessOptions{env: []string{"FAKE_INIT=slow:300"}})
	// Submitted while initializing (within the init timeout): served after
	// init, with its whole timeout.
	if res := h.invoke(`{"op":"echo"}`); res.Kind != KindSuccess {
		t.Fatalf("result = %+v", res)
	}
}

func TestShutdownLetsRunningInvocationFinish(t *testing.T) {
	h := newHarness(t, harnessOptions{grace: 3 * time.Second})
	h.waitState(StateReady)
	job := h.submit(`{"op":"sleep","ms":300}`)
	time.Sleep(50 * time.Millisecond)
	h.stop()
	res, err := job.Wait(context.Background())
	if err != nil || res.Kind != KindSuccess {
		t.Fatalf("result = %+v %v", res, err)
	}
	if h.engine.State() != StateStopping {
		t.Errorf("state = %s", h.engine.State())
	}
}

func TestShutdownGraceExpires(t *testing.T) {
	h := newHarness(t, harnessOptions{grace: 100 * time.Millisecond})
	h.waitState(StateReady)
	job := h.submit(`{"op":"sleep","ms":10000}`)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	h.stop()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("shutdown took %s", d)
	}
	res, err := job.Wait(context.Background())
	if err != nil || res.Kind != KindExit || !strings.Contains(string(res.Payload), "signal: terminated") {
		t.Fatalf("result = %+v %v", res, err)
	}
}

func TestShutdownWhileIdle(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.waitState(StateReady)
	h.stop()
}

func TestShutdownDuringInit(t *testing.T) {
	// The bootstrap never finishes init: no job is taken from the Source,
	// and shutdown still ends promptly.
	h := newHarness(t, harnessOptions{env: []string{"FAKE_INIT=slow:60000"}, initTimeout: time.Hour})
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	h.stop()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("shutdown took %s", d)
	}
}

func TestPoppedJobFailsAfterShutdown(t *testing.T) {
	l := newLoop(New(Options{Log: slog.New(slog.DiscardHandler)}, Dependencies{}, os.Getpid()))
	l.stopping = true
	job := NewJob(Request{RequestID: "x"})
	l.onPopped(popResult{job: job})
	if _, err := job.Wait(context.Background()); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("err = %v", err)
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
}

func TestJobWaitHonorsContext(t *testing.T) {
	job := NewJob(Request{RequestID: "x"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := job.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	job.Fail(ErrShuttingDown)
	job.Fail(errors.New("second outcome is dropped"))
	if _, err := job.Wait(context.Background()); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("err = %v", err)
	}
}

func TestBootstrapNotFound(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	events := make(chan runtimeapi.Event, 16)
	reaper := process.NewReaper(logger)
	defer reaper.Close()
	e := New(Options{Command: []string{"/nonexistent/bootstrap"}, Log: logger, Timeout: time.Second, InitTimeout: time.Second},
		Dependencies{Machine: runtimeapi.NewMachine(events), Events: events, Reaper: reaper,
			Relay: logs.NewRelay(io.Discard, io.Discard), Source: make(chanSource)}, os.Getpid())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.State() != StateError && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.State() != StateError {
		t.Errorf("state = %s", e.State())
	}
	cancel()
	<-done
}

func (h *harness) states() []State {
	h.obs.mu.Lock()
	defer h.obs.mu.Unlock()
	return append([]State(nil), h.obs.states...)
}

func (h *harness) initResults() []bool {
	h.obs.mu.Lock()
	defer h.obs.mu.Unlock()
	return append([]bool(nil), h.obs.inits...)
}

// TestRepeatedCrashesBackOff: consecutive crashes without a result back
// off, and the state says initializing meanwhile, not ready.
func TestRepeatedCrashesBackOff(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.waitState(StateReady)
	if r := h.invoke(`{"op":"exit"}`); r.Kind != KindExit {
		t.Fatalf("first crash = %+v", r)
	}
	if r := h.invoke(`{"op":"exit"}`); r.Kind != KindExit {
		t.Fatalf("second crash = %+v", r)
	}
	// Second crash in a row: re-init delayed by 1 s, state initializing.
	h.waitState(StateInitializing)
	start := time.Now()
	if r := h.invoke(`{"op":"echo"}`); r.Kind != KindSuccess {
		t.Fatalf("after backoff = %+v", r)
	}
	if d := time.Since(start); d < 500*time.Millisecond {
		t.Errorf("no backoff: served after %s", d)
	}
	// A result resets the count: the next crash re-inits at once.
	if r := h.invoke(`{"op":"exit"}`); r.Kind != KindExit {
		t.Fatal(r)
	}
	start = time.Now()
	if r := h.invoke(`{"op":"echo"}`); r.Kind != KindSuccess || time.Since(start) > 900*time.Millisecond {
		t.Errorf("after a single crash: %+v in %s", r, time.Since(start))
	}
}

func TestInitErrorDuringSuppressedInit(t *testing.T) {
	// Slow init past the init timeout, then an init error: the job that
	// started meanwhile gets the init error.
	h := newHarness(t, harnessOptions{env: []string{"FAKE_INIT=slow-error"}, initTimeout: 50 * time.Millisecond})
	res := h.invoke(`{"op":"echo"}`)
	if res.Kind != KindInitError || res.ErrorType != "Runtime.ImportModuleError" {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
}

func TestShutdownDuringInitIsNotAnInitFailure(t *testing.T) {
	h := newHarness(t, harnessOptions{env: []string{"FAKE_INIT=slow:60000"}, initTimeout: time.Hour})
	h.waitState(StateInitializing)
	time.Sleep(50 * time.Millisecond) // let the bootstrap start
	h.stop()
	for _, ok := range h.initResults() {
		if !ok {
			t.Error("stopping during init reported an init failure")
		}
	}
}

func TestLostInvocationRestartsTheBootstrap(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.waitState(StateReady)
	// The engine reacts to EventLost as to a broken bootstrap: kill and
	// re-init; the invocation ends as an exit.
	h.engine.testHook <- func(l *loop) {
		l.onEvent(runtimeapi.Event{Kind: runtimeapi.EventLost, Generation: l.generation, LostID: "x"})
	}
	h.waitState(StateReady)
	if r := h.invoke(`{"op":"echo"}`); r.Kind != KindSuccess {
		t.Errorf("after a lost invocation = %+v", r)
	}
}

func TestUnknownOutcome(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.waitState(StateReady)
	job := h.submit(`{"op":"sleep","ms":2000}`)
	time.Sleep(100 * time.Millisecond)
	h.engine.testHook <- func(l *loop) {
		l.onResult(runtimeapi.Result{ID: job.RequestID, Outcome: runtimeapi.Outcome(99)})
	}
	res, err := job.Wait(context.Background())
	if err != nil || res.Kind != KindError || res.ErrorType != "Runtime.Unknown" {
		t.Errorf("result = %+v %v", res, err)
	}
}

// closedSource is a Source that is closed.
type closedSource struct{ pops int }

func (s *closedSource) Pop(context.Context) (*Job, error) {
	s.pops++
	return nil, ErrSourceClosed
}

func TestClosedSourceStopsPopping(t *testing.T) {
	l := newLoop(New(Options{Log: slog.New(slog.DiscardHandler)}, Dependencies{Source: &closedSource{}}, os.Getpid()))
	l.startPopping()
	l.onPopped(<-l.popped)
	if !l.sourceClosed {
		t.Fatal("source not marked closed")
	}
	l.startPopping()
	if l.popCancel != nil {
		t.Error("popping restarted on a closed source")
	}
}

// blockingSource returns its job only once Pop is cancelled, the race
// that loses a job at shutdown if nobody reads the last pop.
type blockingSource struct{ job *Job }

func (s blockingSource) Pop(ctx context.Context) (*Job, error) {
	<-ctx.Done()
	return s.job, nil
}

func TestJobPoppedAtExitIsFailed(t *testing.T) {
	job := NewJob(Request{RequestID: "x"})
	l := newLoop(New(Options{Log: slog.New(slog.DiscardHandler)}, Dependencies{Source: blockingSource{job}}, os.Getpid()))
	l.startPopping()
	l.failPopped()
	if _, err := job.Wait(context.Background()); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("err = %v", err)
	}
}

func TestPendingJobExpires(t *testing.T) {
	// A job popped but not started (the bootstrap is re-initing) expires at
	// its Expires instead of waiting for the bootstrap.
	h := newHarness(t, harnessOptions{env: []string{"FAKE_INIT=slow:60000"}, initTimeout: time.Hour})
	job := NewJob(Request{RequestID: "x", Expires: time.Now().Add(50 * time.Millisecond)})
	h.engine.testHook <- func(l *loop) { l.setPending(job) }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := job.Wait(ctx); !errors.Is(err, ErrQueueTimeout) {
		t.Errorf("err = %v", err)
	}
}

func TestOOMOfADescendant(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	res := h.invoke(`{"op":"oom-descendant"}`)
	if res.Kind != KindOOM || errorTypeOf(t, res) != "Runtime.OutOfMemory" || !strings.Contains(string(res.Payload), "exit status 137") {
		t.Fatalf("result = %+v %s", res, res.Payload)
	}
}

func TestRetryAfterDuringBackoff(t *testing.T) {
	h := newHarness(t, harnessOptions{env: []string{"FAKE_INIT=exit"}})
	if got := h.engine.RetryAfter(); got != 1 {
		t.Errorf("before any backoff: %d", got)
	}
	h.waitState(StateError)
	// After the first init failure, the re-init waits 1 s; after the
	// second one, 2 s.
	deadline := time.Now().Add(10 * time.Second)
	for h.engine.RetryAfter() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("retry after = %d", h.engine.RetryAfter())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
