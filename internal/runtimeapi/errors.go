// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package runtimeapi

import (
	"errors"
	"net/http"
	"strconv"
)

// ErrNotWaiting is returned by Deliver when no /next is parked.
var ErrNotWaiting = errors.New("runtimeapi: no runtime waiting for an invocation")

// apiError is an error answered to the runtime, in the JSON shape of the
// Runtime API: {"errorMessage": …, "errorType": …}.
type apiError struct {
	status    int
	errorType string
	message   string
}

func (e *apiError) Error() string { return e.message }

// Errors of the state machine. AWS documents 400 for a bad request, 403
// for an invalid state transition, 413 for a too large payload and 500 for
// a state the runtime cannot recover from (it should exit).
var (
	errUnknownID = &apiError{http.StatusBadRequest, "InvalidRequestID",
		"Invalid request ID"}
	errAlreadyResponded = &apiError{http.StatusForbidden, "InvalidStateTransition",
		"State transition from ResponseSent to InvocationResponse failed: the invocation already has a result"}
	errConcurrentNext = &apiError{http.StatusForbidden, "InvalidStateTransition",
		"Another /next request is already waiting"}
	errNextDuringInvocation = &apiError{http.StatusForbidden, "InvalidStateTransition",
		"/next called before the result of the current invocation was posted"}
	errInitErrorAfterInit = &apiError{http.StatusForbidden, "InvalidStateTransition",
		"/init/error is only valid during init"}
	errClosed = &apiError{http.StatusInternalServerError, "Runtime.Unavailable",
		"The runtime is being stopped or restarted; exit"}
	errNotFound = &apiError{http.StatusNotFound, "NotFound",
		"Not found: only the Runtime API 2018-06-01 is implemented"}
	errBodyUnreadable = &apiError{http.StatusBadRequest, "InvalidRequest",
		"Request body could not be read"}
	errSnapStart = &apiError{http.StatusNotFound, "NotFound",
		"SnapStart is not enabled"}
	errMethod = &apiError{http.StatusMethodNotAllowed, "MethodNotAllowed",
		"Method not allowed"}
)

func errTooLarge(limit int64) *apiError {
	return &apiError{http.StatusRequestEntityTooLarge, "RequestEntityTooLarge",
		"Exceeded maximum allowed payload size (" + strconv.FormatInt(limit, 10) + " bytes)."}
}
