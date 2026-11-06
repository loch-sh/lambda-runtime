// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package invokeapi

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// awsError is an error of the Invoke API itself (not a function error), in
// the AWS rest-json shape: x-amzn-ErrorType header and
// {"Type":"User","message":…} body.
type awsError struct {
	status  int
	name    string
	message string
	// retryAfter, in seconds, is set on throttling and shutdown errors.
	retryAfter int
	// reason is the Reason field of TooManyRequestsException.
	reason string
}

func (e *awsError) Error() string { return e.name + ": " + e.message }

func (e *awsError) write(w http.ResponseWriter, requestID string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Amzn-ErrorType", e.name)
	h.Set("X-Amzn-RequestId", requestID)
	if e.retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	doc := map[string]string{"Type": "User", "message": e.message}
	if e.reason != "" {
		doc["Reason"] = e.reason
	}
	if e.status >= 500 {
		doc["Type"] = "Service"
	}
	b, _ := json.Marshal(doc)
	w.WriteHeader(e.status)
	_, _ = w.Write(b)
}

func errNotFound(what string) *awsError {
	return &awsError{status: http.StatusNotFound, name: "ResourceNotFoundException", message: "Function not found: " + what}
}

func errRouteNotFound() *awsError {
	return &awsError{status: http.StatusNotFound, name: "ResourceNotFoundException", message: "Unknown operation"}
}

func errMethod() *awsError {
	return &awsError{status: http.StatusMethodNotAllowed, name: "MethodNotAllowedException", message: "Only POST invokes a function"}
}

func errTooLarge(limit int64) *awsError {
	return &awsError{status: http.StatusRequestEntityTooLarge, name: "RequestTooLargeException",
		message: "Request must be smaller than " + strconv.FormatInt(limit, 10) + " bytes for the InvokeFunction operation"}
}

func errThrottled(retryAfter int) *awsError {
	return &awsError{status: http.StatusTooManyRequests, name: "TooManyRequestsException",
		message: "Rate Exceeded.", retryAfter: retryAfter, reason: "ReservedFunctionConcurrentInvocationLimitExceeded"}
}

func errShuttingDown() *awsError {
	return &awsError{status: http.StatusServiceUnavailable, name: "ServiceException",
		message: "The function is shutting down", retryAfter: 1}
}

func errInvalidParameter(message string) *awsError {
	return &awsError{status: http.StatusBadRequest, name: "InvalidParameterValueException", message: message}
}

func errInvalidContent(message string) *awsError {
	return &awsError{status: http.StatusBadRequest, name: "InvalidRequestContentException", message: message}
}
