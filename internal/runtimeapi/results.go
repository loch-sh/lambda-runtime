// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package runtimeapi

import "context"

// Upload tracks the body of a result being read for the current
// invocation; End it once the body is read and the result recorded.
type Upload struct {
	m          *Machine
	generation uint64
}

// BeginResult checks that id is the current invocation before its body is
// read, and records the upload so that a concurrent /next waits for it.
func (m *Machine) BeginResult(id string) (*Upload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkLocked(id); err != nil {
		return nil, err
	}
	m.uploads++
	return &Upload{m: m, generation: m.generation}, nil
}

// End ends the upload. It is a no-op after a Reset or Close.
func (u *Upload) End() {
	m := u.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if u.generation == m.generation && m.uploads > 0 {
		m.uploads--
		m.notifyLocked()
	}
}

func (m *Machine) checkLocked(id string) error {
	switch {
	case m.phase == phaseClosed:
		return errClosed
	case m.current != nil && m.current.ID == id:
		return nil
	case id != "" && id == m.lastDone:
		return errAlreadyResponded
	default:
		return errUnknownID
	}
}

// Respond records a successful response (POST /invocation/{id}/response).
func (m *Machine) Respond(ctx context.Context, id string, body []byte, contentType string) error {
	return m.complete(ctx, Result{ID: id, Outcome: OutcomeSuccess, Body: body, ContentType: contentType})
}

// Fail records a function error (POST /invocation/{id}/error).
func (m *Machine) Fail(ctx context.Context, id, errorType string, body []byte) error {
	return m.complete(ctx, Result{ID: id, Outcome: OutcomeFunctionError, Body: body, ErrorType: errorType})
}

// TooLarge records a response that exceeded the payload limit.
func (m *Machine) TooLarge(ctx context.Context, id string) error {
	return m.complete(ctx, Result{ID: id, Outcome: OutcomeResponseTooLarge})
}

// complete records the result of the current invocation. Once recorded,
// the result always reaches the owner, even if ctx ends meanwhile.
func (m *Machine) complete(ctx context.Context, res Result) error {
	if !m.lockSending(ctx) {
		return ctx.Err()
	}
	defer m.unlockSending()
	m.mu.Lock()
	if err := m.checkLocked(res.ID); err != nil {
		m.mu.Unlock()
		return err
	}
	m.current = nil
	m.lastDone = res.ID
	m.phase = phaseIdle
	m.notifyLocked()
	gen := m.generation
	m.mu.Unlock()

	m.mustEmit(Event{Kind: EventResult, Generation: gen, Result: res})
	return nil
}

// CheckInit reports whether /init/error is acceptable now, without
// changing anything; used to validate a request before reading its body.
func (m *Machine) CheckInit() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkInitLocked()
}

// InitFailed records an init error (POST /runtime/init/error). The machine
// closes: the owner is expected to stop the runtime and Reset. Once
// recorded, the error always reaches the owner.
func (m *Machine) InitFailed(ctx context.Context, errorType string, body []byte) error {
	if !m.lockSending(ctx) {
		return ctx.Err()
	}
	defer m.unlockSending()
	m.mu.Lock()
	if err := m.checkInitLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.releaseLocked()
	m.phase = phaseClosed
	gen := m.generation
	m.mu.Unlock()

	m.mustEmit(Event{Kind: EventInitError, Generation: gen, InitError: &InitError{ErrorType: errorType, Body: body}})
	return nil
}

func (m *Machine) checkInitLocked() error {
	switch m.phase {
	case phaseClosed:
		return errClosed
	case phaseInitializing:
		return nil
	}
	return errInitErrorAfterInit
}
