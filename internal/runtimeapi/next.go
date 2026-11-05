// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package runtimeapi

import "context"

// waiter is a parked /next.
type waiter struct {
	invocation chan *Invocation
	closed     chan struct{}
	// initial is set when the waiter is the first /next of a generation.
	initial bool
}

// Next parks until an invocation is delivered (GET /invocation/next).
func (m *Machine) Next(ctx context.Context) (*Invocation, error) {
	if err := m.waitUploads(ctx); err != nil {
		return nil, err
	}
	if !m.lockSending(ctx) {
		return nil, ctx.Err()
	}
	w, gen, err := m.park()
	if err != nil {
		m.unlockSending()
		return nil, err
	}
	sent := m.emit(ctx, Event{Kind: EventIdle, Generation: gen, Initial: w.initial})
	m.unlockSending()
	if !sent {
		m.abandonWaiter(w)
		return nil, ctx.Err()
	}
	select {
	case inv := <-w.invocation:
		return inv, nil
	case <-w.closed:
		return nil, errClosed
	case <-ctx.Done():
		if inv := m.abandonWaiter(w); inv != nil {
			m.mustEmit(Event{Kind: EventLost, Generation: gen, LostID: inv.ID})
			return nil, &LostInvocationError{ID: inv.ID}
		}
		return nil, ctx.Err()
	}
}

// waitUploads holds a /next sent while the result of the current
// invocation is still being uploaded: the Node.js client may call /next
// before the 202 of a streamed response. It returns when no upload is in
// progress or the state changed.
func (m *Machine) waitUploads(ctx context.Context) error {
	for {
		m.mu.Lock()
		if m.phase != phaseInvoking || m.uploads == 0 {
			m.mu.Unlock()
			return nil
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// park registers a waiter for /next, or tells why the runtime may not
// wait now.
func (m *Machine) park() (*waiter, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.phase {
	case phaseClosed:
		return nil, 0, errClosed
	case phaseWaiting:
		return nil, 0, errConcurrentNext
	case phaseInvoking:
		return nil, 0, errNextDuringInvocation
	}
	w := &waiter{
		invocation: make(chan *Invocation, 1),
		closed:     make(chan struct{}),
		initial:    m.phase == phaseInitializing,
	}
	m.waiter = w
	m.phase = phaseWaiting
	return w, m.generation, nil
}

// abandonWaiter unparks w after its client went away, restoring the phase
// it was parked from. It returns the invocation delivered to w in the
// meantime, if any.
func (m *Machine) abandonWaiter(w *waiter) *Invocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waiter == w {
		m.waiter = nil
		m.phase = phaseIdle
		if w.initial {
			m.phase = phaseInitializing
		}
		return nil
	}
	select {
	case inv := <-w.invocation:
		return inv
	default:
		return nil
	}
}

// LostInvocationError is returned by Next when an invocation was delivered
// but the runtime's request ended before it could be written.
type LostInvocationError struct{ ID string }

func (e *LostInvocationError) Error() string {
	return "invocation " + e.ID + " was delivered to a /next whose client went away"
}
