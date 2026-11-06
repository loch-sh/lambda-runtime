// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package invokeapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/ids"
)

const (
	// maxClientContext is the AWS limit of X-Amz-Client-Context (base64).
	maxClientContext = 3583
	latest           = "$LATEST"
)

// invocationType values of X-Amz-Invocation-Type.
const (
	typeRequestResponse = "RequestResponse"
	typeDryRun          = "DryRun"
	typeEvent           = "Event"
)

// FunctionName tells which names designate the function.
type FunctionName struct {
	Name   string
	Region string
}

// fakeAccount is the account id of the ARN given to the runtime.
const fakeAccount = "000000000000"

// matches reports whether the {name} of the path designates the function:
// "function" (the RIE convention), its name, name:$LATEST, or its full
// (arn:aws:lambda:<region>:000000000000:function:<name>) or partial
// (000000000000:function:<name>) ARN.
func (f FunctionName) matches(name string) bool {
	name, qualifier := splitQualifier(name)
	if qualifier != "" && qualifier != latest {
		return false
	}
	if name == "function" || name == f.Name {
		return true
	}
	// arn:aws:lambda:<region>:<account>:function:<name> or
	// <account>:function:<name>.
	parts := strings.Split(name, ":")
	switch {
	case len(parts) == 7:
		return parts[0] == "arn" && parts[1] == "aws" && parts[2] == "lambda" && parts[3] == f.Region &&
			parts[4] == fakeAccount && parts[5] == "function" && parts[6] == f.Name
	case len(parts) == 3:
		return parts[0] == fakeAccount && parts[1] == "function" && parts[2] == f.Name
	}
	return false
}

// splitQualifier separates a trailing ":$LATEST" or ":<version>" from a
// name or ARN.
func splitQualifier(name string) (string, string) {
	parts := strings.Split(name, ":")
	switch {
	case len(parts) == 2, len(parts) == 4 && parts[1] == "function", len(parts) == 8 && parts[0] == "arn":
		return strings.Join(parts[:len(parts)-1], ":"), parts[len(parts)-1]
	}
	return name, ""
}

// invocationType validates X-Amz-Invocation-Type.
func invocationType(r *http.Request) (string, *awsError) {
	switch t := r.Header.Get("X-Amz-Invocation-Type"); t {
	case "", typeRequestResponse:
		return typeRequestResponse, nil
	case typeDryRun:
		return typeDryRun, nil
	case typeEvent:
		return "", errInvalidParameter("Asynchronous (Event) invocations are not supported by this runtime")
	default:
		return "", errInvalidParameter("1 validation error detected: Value at 'invocationType' failed to satisfy constraint")
	}
}

// clientContext decodes X-Amz-Client-Context: base64 of a JSON object.
func clientContext(r *http.Request) (string, *awsError) {
	v := r.Header.Get("X-Amz-Client-Context")
	if v == "" {
		return "", nil
	}
	if len(v) > maxClientContext {
		return "", errInvalidContent("Client context must be smaller than 3583 bytes")
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", errInvalidContent("Client context must be base64 encoded")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", errInvalidContent("Client context must be a JSON object")
	}
	// Compacted: no raw line breaks in the header relayed to the runtime.
	var compact bytes.Buffer
	_ = json.Compact(&compact, raw)
	return compact.String(), nil
}

var (
	tenantPattern      = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)
	contentTypePattern = regexp.MustCompile(`^[\w.+-]+/[\w.+-]+(; ?[ -~]{0,100})?$`)
)

// tenantID returns X-Amz-Tenant-Id when it is well formed, "" otherwise.
func tenantID(r *http.Request) string {
	if v := r.Header.Get("X-Amz-Tenant-Id"); tenantPattern.MatchString(v) {
		return v
	}
	return ""
}

// safeContentType relays the content type posted by the runtime only when
// it is a plain, short media type; anything else becomes application/json,
// what Lambda always answers.
func safeContentType(ct string) string {
	if len(ct) <= 128 && contentTypePattern.MatchString(ct) {
		return ct
	}
	return "application/json"
}

// requestID returns a new request id. As on Lambda, a caller cannot choose
// it: two callers sharing an id would blur the correlation of the logs.
func requestID() string { return ids.NewRequestID() }

// traceID returns the caller's X-Amzn-Trace-Id when it is well formed, a
// new unsampled one otherwise.
func traceID(r *http.Request, now time.Time) string {
	if v := r.Header.Get("X-Amzn-Trace-Id"); ids.ValidTraceID(v) {
		return v
	}
	return ids.NewTraceID(now)
}

// wantsTail reports whether the caller asked for the log tail.
func wantsTail(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("X-Amz-Log-Type"), "Tail")
}
