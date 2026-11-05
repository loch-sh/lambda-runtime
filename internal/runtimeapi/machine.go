// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package runtimeapi implements the AWS Lambda Runtime API (2018-06-01)
// spoken by the bootstrap of a Lambda image: a state machine (Machine)
// and its HTTP front (Handler).
//
// Every request the runtime sends is checked against the current state.
// An unknown or stale request id, a second response, or a concurrent
// /next yields a 4xx error; nothing a runtime sends can crash the server.
package runtimeapi

import (
	"context"
	"sync"
)

type phase int

const (
	// phaseClosed: no runtime is expected (before Reset, after Close).
	phaseClosed phase = iota
	// phaseInitializing: a runtime was started, no /next yet.
	phaseInitializing
	// phaseIdle: init done, nothing in progress, no /next parked.
	phaseIdle
	// phaseWaiting: a /next is parked.
	phaseWaiting
	// phaseInvoking: an invocation was handed out, no result yet.
	phaseInvoking
)

// Machine is the state machine of the Runtime API. It is safe for
// concurrent use.
//
// The owner must keep receiving from the events channel: results and init
// errors are always delivered, whatever happens to the HTTP request that
// carried them, so that a result the runtime saw accepted is never lost.
type Machine struct {
	events chan<- Event
	// sending orders the events: a state change and the event announcing
	// it happen under it, so the owner sees events in transition order
	// even when the runtime sends /response and /next concurrently.
	sending chan struct{}

	mu         sync.Mutex
	generation uint64
	phase      phase
	waiter     *waiter
	current    *Invocation
	// lastDone is the id of the last completed invocation, to tell a
	// duplicate result from an unknown id.
	lastDone string
	// uploads counts the result bodies of the current invocation being
	// read; a /next waits for them instead of being refused.
	uploads int
	// changed is closed and replaced whenever the state changes, to wake
	// up the /next waiting for an upload.
	changed chan struct{}
}

// NewMachine returns a closed machine reporting to events.
func NewMachine(events chan<- Event) *Machine {
	return &Machine{events: events, sending: make(chan struct{}, 1), changed: make(chan struct{})}
}

// Reset prepares the machine for a newly started runtime and returns the
// new generation. A parked /next of the previous runtime is released.
func (m *Machine) Reset() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseLocked()
	m.generation++
	m.phase = phaseInitializing
	return m.generation
}

// Close makes every runtime request fail until the next Reset; used when
// the runtime is being killed.
func (m *Machine) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseLocked()
	m.phase = phaseClosed
}

func (m *Machine) releaseLocked() {
	if m.waiter != nil {
		close(m.waiter.closed)
		m.waiter = nil
	}
	m.current = nil
	m.lastDone = ""
	m.uploads = 0
	m.notifyLocked()
}

// notifyLocked wakes up whoever waits for a state change.
func (m *Machine) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// Deliver hands inv to the /next parked for generation gen. It fails with
// ErrNotWaiting when no /next of that generation is parked.
func (m *Machine) Deliver(gen uint64, inv *Invocation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.phase != phaseWaiting || m.generation != gen {
		return ErrNotWaiting
	}
	m.waiter.invocation <- inv // buffered, never blocks
	m.waiter = nil
	m.current = inv
	m.phase = phaseInvoking
	return nil
}

// lockSending serializes the state changes that emit an event; it gives
// up when ctx ends, before anything changed.
func (m *Machine) lockSending(ctx context.Context) bool {
	select {
	case m.sending <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *Machine) unlockSending() { <-m.sending }

// emit sends ev to the owner, giving up when ctx ends. Only EventIdle may
// be given up (the /next is then abandoned): results and init errors use
// mustEmit.
func (m *Machine) emit(ctx context.Context, ev Event) bool {
	select {
	case m.events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// mustEmit sends ev to the owner whatever happens to the request.
func (m *Machine) mustEmit(ev Event) { m.events <- ev }
