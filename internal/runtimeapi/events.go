// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package runtimeapi

import "time"

// Invocation is one event handed to the runtime.
type Invocation struct {
	ID              string
	Payload         []byte
	Deadline        time.Time
	FunctionARN     string
	TraceID         string
	ClientContext   string
	CognitoIdentity string
	TenantID        string
}

// Outcome classifies a result posted by the runtime.
type Outcome int

// Outcomes.
const (
	// OutcomeSuccess: /response.
	OutcomeSuccess Outcome = iota
	// OutcomeFunctionError: /error, or a streamed response that ended with
	// an error trailer.
	OutcomeFunctionError
	// OutcomeResponseTooLarge: the response exceeded the payload limit.
	OutcomeResponseTooLarge
)

// Result is what the runtime answered for an invocation.
type Result struct {
	ID          string
	Outcome     Outcome
	Body        []byte
	ContentType string
	// ErrorType is the error type of a function error.
	ErrorType string
}

// EventKind tells what happened on the Runtime API.
type EventKind int

// Event kinds.
const (
	// EventIdle: the runtime waits in /next and can receive an invocation.
	// Initial is set on the first /next of a generation (init complete);
	// it may come twice if that /next was abandoned and retried.
	EventIdle EventKind = iota
	// EventInitError: the runtime posted /init/error.
	EventInitError
	// EventResult: the runtime posted the result of the current invocation.
	EventResult
	// EventLost: an invocation was handed to a /next whose client went away
	// before it was written. The runtime is broken: the owner should
	// restart it. LostID names the invocation.
	EventLost
)

// Event is sent by the Machine to its owner.
type Event struct {
	Kind EventKind
	// Generation is the value returned by the Reset that preceded the event;
	// events of older generations must be ignored.
	Generation uint64
	Initial    bool
	Result     Result
	// InitError is set for EventInitError.
	InitError *InitError
	// LostID is set for EventLost.
	LostID string
}

// InitError is the error a runtime posted to /init/error.
type InitError struct {
	ErrorType string
	Body      []byte
}
