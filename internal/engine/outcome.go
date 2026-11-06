// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/process"
)

// Error types of the results the runtime produces itself.
const (
	errorTypeTimeout  = "Sandbox.Timedout"
	errorTypeExit     = "Runtime.ExitError"
	errorTypeOOM      = "Runtime.OutOfMemory"
	errorTypeTooLarge = "Function.ResponseSizeTooLarge"
)

// errorDocument is the Lambda error shape {"errorType", "errorMessage"}.
func errorDocument(errorType, message string) []byte {
	b, _ := json.Marshal(struct {
		ErrorType    string `json:"errorType"`
		ErrorMessage string `json:"errorMessage"`
	}{errorType, message})
	return b
}

func timeoutResult(id string, timeout time.Duration) *Result {
	msg := fmt.Sprintf("RequestId: %s Error: Task timed out after %.2f seconds", id, timeout.Seconds())
	return errorResult(id, KindTimeout, errorTypeTimeout, msg)
}

func exitResult(id string, status process.ExitStatus) *Result {
	msg := "RequestId: " + id + " Error: Runtime exited with error: " + status.String()
	return errorResult(id, KindExit, errorTypeExit, msg)
}

func oomResult(id string, status process.ExitStatus) *Result {
	msg := "RequestId: " + id + " Error: Runtime exited with error: " + status.String()
	return errorResult(id, KindOOM, errorTypeOOM, msg)
}

func tooLargeResult(id string, limit int64) *Result {
	msg := "Response payload size exceeded maximum allowed payload size (" + strconv.FormatInt(limit, 10) + " bytes)."
	return errorResult(id, KindTooLarge, errorTypeTooLarge, msg)
}

func errorResult(id string, kind Kind, errorType, msg string) *Result {
	return &Result{
		RequestID:   id,
		Kind:        kind,
		Payload:     errorDocument(errorType, msg),
		ContentType: "application/json",
		ErrorType:   errorType,
	}
}

// initFailure is why the last init failed; the jobs served while the
// engine is in the error state get it as their result.
type initFailure struct {
	errorType string
	body      []byte
}

// exitInitFailure is the init failure of a runtime that exited before its
// first /next without posting /init/error.
func exitInitFailure(status process.ExitStatus) *initFailure {
	return &initFailure{
		errorType: errorTypeExit,
		body:      errorDocument(errorTypeExit, "Runtime exited with error: "+status.String()),
	}
}

func startInitFailure(err error) *initFailure {
	return &initFailure{
		errorType: errorTypeExit,
		body:      errorDocument(errorTypeExit, "Runtime could not be started: "+err.Error()),
	}
}

func (f *initFailure) result(id string) *Result {
	return &Result{
		RequestID:   id,
		Kind:        KindInitError,
		Payload:     f.body,
		ContentType: "application/json",
		ErrorType:   f.errorType,
	}
}
