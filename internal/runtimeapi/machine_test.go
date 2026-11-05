// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package runtimeapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

var bg = context.Background()

// check is BeginResult without the upload, for the tests of validation.
func (m *Machine) check(id string) error {
	u, err := m.BeginResult(id)
	if err == nil {
		u.End()
	}
	return err
}

// deliver delivers to the current generation.
func (f *fixture) deliver(inv *Invocation) error {
	f.m.mu.Lock()
	gen := f.m.generation
	f.m.mu.Unlock()
	return f.m.Deliver(gen, inv)
}

// fixture runs a Machine with a buffered event channel.
type fixture struct {
	t      *testing.T
	m      *Machine
	events chan Event
}

func newFixture(t *testing.T) *fixture {
	events := make(chan Event, 16)
	return &fixture{t: t, m: NewMachine(events), events: events}
}

func (f *fixture) event() Event {
	f.t.Helper()
	select {
	case e := <-f.events:
		return e
	case <-time.After(5 * time.Second):
		f.t.Fatal("no event")
		return Event{}
	}
}

func (f *fixture) noEvent() {
	f.t.Helper()
	select {
	case e := <-f.events:
		f.t.Fatalf("unexpected event %+v", e)
	default:
	}
}

type nextResult struct {
	inv *Invocation
	err error
}

// park starts a /next in the background and waits until it is parked.
func (f *fixture) park(ctx context.Context) (<-chan nextResult, Event) {
	f.t.Helper()
	ch := make(chan nextResult, 1)
	go func() {
		inv, err := f.m.Next(ctx)
		ch <- nextResult{inv, err}
	}()
	e := f.event()
	if e.Kind != EventIdle {
		f.t.Fatalf("event = %+v, want idle", e)
	}
	return ch, e
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.status != status {
		t.Fatalf("err = %v, want API error %d", err, status)
	}
}

func recv(t *testing.T, ch <-chan nextResult) nextResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("next did not return")
		return nextResult{}
	}
}

func TestInvocationCycle(t *testing.T) {
	f := newFixture(t)
	gen := f.m.Reset()

	ch, e := f.park(context.Background())
	if !e.Initial || e.Generation != gen {
		t.Errorf("first idle = %+v", e)
	}
	inv := &Invocation{ID: "id-1", Payload: []byte(`{}`)}
	if err := f.deliver(inv); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, ch); got.err != nil || got.inv != inv {
		t.Fatalf("next = %+v", got)
	}
	if err := f.m.Respond(bg, "id-1", []byte(`"ok"`), "application/json"); err != nil {
		t.Fatal(err)
	}
	e = f.event()
	if e.Kind != EventResult || e.Result.Outcome != OutcomeSuccess || string(e.Result.Body) != `"ok"` || e.Result.ContentType != "application/json" {
		t.Errorf("result = %+v", e)
	}

	// Second invocation: not initial; ends with a function error.
	ch, e = f.park(context.Background())
	if e.Initial {
		t.Error("second idle must not be initial")
	}
	if err := f.deliver(&Invocation{ID: "id-2"}); err != nil {
		t.Fatal(err)
	}
	recv(t, ch)
	if err := f.m.Fail(bg, "id-2", "Error", []byte(`{"errorType":"Error"}`)); err != nil {
		t.Fatal(err)
	}
	if e := f.event(); e.Result.Outcome != OutcomeFunctionError || e.Result.ErrorType != "Error" {
		t.Errorf("result = %+v", e)
	}

	// Third: too large.
	ch, _ = f.park(context.Background())
	_ = f.deliver(&Invocation{ID: "id-3"})
	recv(t, ch)
	if err := f.m.TooLarge(bg, "id-3"); err != nil {
		t.Fatal(err)
	}
	if e := f.event(); e.Result.Outcome != OutcomeResponseTooLarge || e.Result.ID != "id-3" {
		t.Errorf("result = %+v", e)
	}
}

func TestInvalidTransitions(t *testing.T) {
	f := newFixture(t)

	// Closed before the first Reset.
	_, err := f.m.Next(context.Background())
	wantStatus(t, err, http.StatusInternalServerError)
	wantStatus(t, f.m.Respond(bg, "x", nil, ""), http.StatusInternalServerError)
	wantStatus(t, f.m.InitFailed(bg, "E", nil), http.StatusInternalServerError)

	f.m.Reset()
	// No result before an invocation.
	wantStatus(t, f.m.Respond(bg, "x", nil, ""), http.StatusBadRequest)
	wantStatus(t, f.m.Respond(bg, "", nil, ""), http.StatusBadRequest)
	if err := f.deliver(&Invocation{ID: "x"}); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("deliver without next: %v", err)
	}

	ch, _ := f.park(context.Background())
	// Concurrent next.
	_, err = f.m.Next(context.Background())
	wantStatus(t, err, http.StatusForbidden)
	// init/error after init.
	wantStatus(t, f.m.InitFailed(bg, "E", nil), http.StatusForbidden)

	_ = f.deliver(&Invocation{ID: "a"})
	recv(t, ch)
	// Second Deliver while invoking.
	if err := f.deliver(&Invocation{ID: "b"}); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("deliver while invoking: %v", err)
	}
	// next before the result.
	_, err = f.m.Next(context.Background())
	wantStatus(t, err, http.StatusForbidden)
	// Wrong id.
	wantStatus(t, f.m.Respond(bg, "b", nil, ""), http.StatusBadRequest)
	if err := f.m.check("a"); err != nil {
		t.Errorf("check current: %v", err)
	}

	if err := f.m.Respond(bg, "a", nil, ""); err != nil {
		t.Fatal(err)
	}
	f.event()
	// Duplicate result, as response or as error.
	wantStatus(t, f.m.Respond(bg, "a", nil, ""), http.StatusForbidden)
	wantStatus(t, f.m.Fail(bg, "a", "E", nil), http.StatusForbidden)
	wantStatus(t, f.m.check("a"), http.StatusForbidden)
	f.noEvent()
}

func TestResetReleasesParkedNextAndForgetsIDs(t *testing.T) {
	f := newFixture(t)
	gen1 := f.m.Reset()
	ch, _ := f.park(context.Background())
	_ = f.deliver(&Invocation{ID: "old"})
	recv(t, ch)

	gen2 := f.m.Reset()
	if gen2 != gen1+1 {
		t.Errorf("generation %d -> %d", gen1, gen2)
	}
	// The result of the previous runtime is now unknown.
	wantStatus(t, f.m.Respond(bg, "old", nil, ""), http.StatusBadRequest)

	ch, e := f.park(context.Background())
	if !e.Initial || e.Generation != gen2 {
		t.Errorf("idle after reset = %+v", e)
	}
	f.m.Reset()
	wantStatus(t, recv(t, ch).err, http.StatusInternalServerError)
}

func TestCloseFailsEverything(t *testing.T) {
	f := newFixture(t)
	f.m.Reset()
	ch, _ := f.park(context.Background())
	f.m.Close()
	wantStatus(t, recv(t, ch).err, http.StatusInternalServerError)
	_, err := f.m.Next(context.Background())
	wantStatus(t, err, http.StatusInternalServerError)
	wantStatus(t, f.m.check("x"), http.StatusInternalServerError)
}

func TestAbandonedNext(t *testing.T) {
	f := newFixture(t)
	f.m.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := f.park(ctx)
	cancel()
	if got := recv(t, ch); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("next = %+v", got)
	}
	if err := f.deliver(&Invocation{ID: "x"}); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("deliver after abandon: %v", err)
	}
	// The runtime may park again.
	f.park(context.Background())
}

func TestAbandonAfterDeliveryKeepsInvocation(t *testing.T) {
	f := newFixture(t)
	f.m.Reset()
	// Park without a reader, so the delivered invocation stays in the
	// waiter when it is abandoned.
	w, _, err := f.m.park()
	if err != nil {
		t.Fatal(err)
	}
	_ = f.deliver(&Invocation{ID: "x"})
	if inv := f.m.abandonWaiter(w); inv == nil || inv.ID != "x" {
		t.Errorf("abandon after delivery must return the invocation, got %+v", inv)
	}
	if err := f.m.check("x"); err != nil {
		t.Errorf("delivered invocation must stay current: %v", err)
	}
}

func TestAbandonedFirstNextKeepsInitPending(t *testing.T) {
	f := newFixture(t)
	f.m.Reset()
	ctx, cancel := context.WithCancel(bg)
	ch, e := f.park(ctx)
	if !e.Initial {
		t.Fatal("first next must be initial")
	}
	cancel()
	recv(t, ch)
	// The retry still completes init, and /init/error is still allowed.
	_, e = f.park(bg)
	if !e.Initial {
		t.Error("retried first next must still be initial")
	}
}

func TestStalledOwnerDoesNotWedgeNext(t *testing.T) {
	m := NewMachine(make(chan Event)) // nobody receives
	m.Reset()
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	if _, err := m.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("next with a stalled owner: %v", err)
	}
	// The waiter was removed: init is still pending.
	if m.phase != phaseInitializing {
		t.Errorf("phase = %v", m.phase)
	}
}

// A result accepted by the machine reaches the owner even when the request
// that carried it ends before the owner receives it.
func TestResultNotLostWhenRequestEnds(t *testing.T) {
	events := make(chan Event) // unbuffered: the owner is busy
	m := NewMachine(events)
	gen := m.Reset()
	go func() { _, _ = m.Next(bg) }()
	<-events // idle
	if err := m.Deliver(gen, &Invocation{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	errc := make(chan error, 1)
	go func() { errc <- m.Respond(ctx, "a", []byte("ok"), "") }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	// The runtime got its 202, then parks again: the owner must see the
	// result first.
	go func() { _, _ = m.Next(bg) }()
	if e := <-events; e.Kind != EventResult || string(e.Result.Body) != "ok" {
		t.Fatalf("first event = %+v", e)
	}
	if err := <-errc; err != nil {
		t.Errorf("respond: %v", err)
	}
	if e := <-events; e.Kind != EventIdle {
		t.Errorf("second event = %+v", e)
	}
	m.Close()
}

func TestDeliverChecksGeneration(t *testing.T) {
	f := newFixture(t)
	old := f.m.Reset()
	gen := f.m.Reset()
	f.park(bg)
	if err := f.m.Deliver(old, &Invocation{ID: "x"}); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("deliver to a previous generation: %v", err)
	}
	if err := f.m.Deliver(gen, &Invocation{ID: "x"}); err != nil {
		t.Errorf("deliver: %v", err)
	}
}

func TestNextWaitsForResultUpload(t *testing.T) {
	f := newFixture(t)
	f.m.Reset()
	ch, _ := f.park(bg)
	_ = f.deliver(&Invocation{ID: "a"})
	recv(t, ch)
	upload, err := f.m.BeginResult("a")
	if err != nil {
		t.Fatal(err)
	}
	// /next while the response is being uploaded: held, not refused.
	next := make(chan nextResult, 1)
	go func() {
		inv, err := f.m.Next(bg)
		next <- nextResult{inv, err}
	}()
	select {
	case r := <-next:
		t.Fatalf("next returned during the upload: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	if err := f.m.Respond(bg, "a", nil, ""); err != nil {
		t.Fatal(err)
	}
	upload.End()
	if e := f.event(); e.Kind != EventResult {
		t.Fatalf("event = %+v", e)
	}
	if e := f.event(); e.Kind != EventIdle {
		t.Fatalf("event = %+v", e)
	}
	_ = f.deliver(&Invocation{ID: "b"})
	if r := recv(t, next); r.err != nil || r.inv.ID != "b" {
		t.Errorf("next = %+v", r)
	}
}

func TestUploadAbortedReleasesNext(t *testing.T) {
	f := newFixture(t)
	f.m.Reset()
	ch, _ := f.park(bg)
	_ = f.deliver(&Invocation{ID: "a"})
	recv(t, ch)
	upload, _ := f.m.BeginResult("a")
	next := make(chan nextResult, 1)
	go func() {
		inv, err := f.m.Next(bg)
		next <- nextResult{inv, err}
	}()
	time.Sleep(20 * time.Millisecond)
	// The upload failed without a result: /next is then refused as before.
	upload.End()
	wantStatus(t, recv(t, next).err, http.StatusForbidden)
	// Ending twice, or after a Reset, is harmless.
	upload.End()
	u2, _ := f.m.BeginResult("a")
	f.m.Reset()
	u2.End()
}

func TestResetDuringUploadReleasesNext(t *testing.T) {
	f := newFixture(t)
	f.m.Reset()
	ch, _ := f.park(bg)
	_ = f.deliver(&Invocation{ID: "a"})
	recv(t, ch)
	if _, err := f.m.BeginResult("a"); err != nil {
		t.Fatal(err)
	}
	next := make(chan nextResult, 1)
	go func() {
		inv, err := f.m.Next(bg)
		next <- nextResult{inv, err}
	}()
	time.Sleep(20 * time.Millisecond)
	f.m.Close()
	wantStatus(t, recv(t, next).err, http.StatusInternalServerError)
}

func TestNextWaitingForUploadHonorsContext(t *testing.T) {
	f := newFixture(t)
	f.m.Reset()
	ch, _ := f.park(bg)
	_ = f.deliver(&Invocation{ID: "a"})
	recv(t, ch)
	_, _ = f.m.BeginResult("a")
	ctx, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancel()
	if _, err := f.m.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

func TestLostInvocationIsReported(t *testing.T) {
	f := newFixture(t)
	f.m.Reset()
	ctx, cancel := context.WithCancel(bg)
	ch, _ := f.park(ctx)
	// Deliver and cancel together until Next reports the loss (the select
	// may also pick the invocation, which is then returned normally).
	_ = f.deliver(&Invocation{ID: "x"})
	cancel()
	got := recv(t, ch)
	var lost *LostInvocationError
	if errors.As(got.err, &lost) {
		if e := f.event(); e.Kind != EventLost || e.LostID != "x" {
			t.Errorf("event = %+v", e)
		}
	} else if got.inv == nil {
		t.Fatalf("next = %+v", got)
	}
}

func TestInitError(t *testing.T) {
	f := newFixture(t)
	gen := f.m.Reset()
	if err := f.m.InitFailed(bg, "Runtime.ImportModuleError", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	e := f.event()
	if e.Kind != EventInitError || e.Generation != gen || e.InitError.ErrorType != "Runtime.ImportModuleError" || string(e.InitError.Body) != `{"x":1}` {
		t.Errorf("event = %+v", e)
	}
	// The machine is closed until the next Reset.
	_, err := f.m.Next(context.Background())
	wantStatus(t, err, http.StatusInternalServerError)
	wantStatus(t, f.m.InitFailed(bg, "E", nil), http.StatusInternalServerError)
}
