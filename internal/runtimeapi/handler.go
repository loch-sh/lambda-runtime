// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package runtimeapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// Runtime API header names.
const (
	headerRequestID       = "Lambda-Runtime-Aws-Request-Id"
	headerDeadline        = "Lambda-Runtime-Deadline-Ms"
	headerFunctionARN     = "Lambda-Runtime-Invoked-Function-Arn"
	headerTraceID         = "Lambda-Runtime-Trace-Id"
	headerClientContext   = "Lambda-Runtime-Client-Context"
	headerCognitoIdentity = "Lambda-Runtime-Cognito-Identity"
	headerTenantID        = "Lambda-Runtime-Aws-Tenant-Id"
	headerErrorType       = "Lambda-Runtime-Function-Error-Type"
	headerResponseMode    = "Lambda-Runtime-Function-Response-Mode"
	// trailerErrorBody carries the base64 error of a streamed response.
	trailerErrorBody = "Lambda-Runtime-Function-Error-Body"

	// maxErrorTypeLen bounds the error type kept from the runtime.
	maxErrorTypeLen = 256
	// unknownErrorType is used when the runtime gives no error type.
	unknownErrorType = "Runtime.Unknown"
)

// Handler serves the Runtime API over HTTP on top of a Machine.
type Handler struct {
	machine    *Machine
	maxPayload int64
	log        *slog.Logger
	mux        *http.ServeMux
	// reading lets one body read at a time hold up to maxPayload in
	// memory: concurrent posts for the current id wait their turn.
	reading chan struct{}
}

// NewHandler returns the HTTP handler of the Runtime API. maxPayload
// bounds the response and error bodies.
func NewHandler(m *Machine, maxPayload int64, log *slog.Logger) *Handler {
	h := &Handler{machine: m, maxPayload: maxPayload, log: log, mux: http.NewServeMux(), reading: make(chan struct{}, 1)}
	h.mux.HandleFunc("/2018-06-01/runtime/invocation/next", h.only(http.MethodGet, h.next))
	h.mux.HandleFunc("/2018-06-01/runtime/invocation/{id}/response", h.only(http.MethodPost, h.response))
	h.mux.HandleFunc("/2018-06-01/runtime/invocation/{id}/error", h.only(http.MethodPost, h.invocationError))
	h.mux.HandleFunc("/2018-06-01/runtime/init/error", h.only(http.MethodPost, h.initError))
	h.mux.HandleFunc("/2018-06-01/runtime/restore/next", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, errSnapStart)
	})
	// Extensions, Telemetry, Logs APIs and anything else: out of scope.
	h.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { writeError(w, errNotFound) })
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) only(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeError(w, errMethod)
			return
		}
		next(w, r)
	}
}

// next serves GET /2018-06-01/runtime/invocation/next.
func (h *Handler) next(w http.ResponseWriter, r *http.Request) {
	inv, err := h.machine.Next(r.Context())
	if err != nil {
		h.reject(w, "next", "", err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set(headerRequestID, inv.ID)
	hd.Set(headerDeadline, strconv.FormatInt(inv.Deadline.UnixMilli(), 10))
	hd.Set(headerFunctionARN, inv.FunctionARN)
	hd.Set(headerTraceID, inv.TraceID)
	if inv.ClientContext != "" {
		hd.Set(headerClientContext, inv.ClientContext)
	}
	if inv.CognitoIdentity != "" {
		hd.Set(headerCognitoIdentity, inv.CognitoIdentity)
	}
	if inv.TenantID != "" {
		hd.Set(headerTenantID, inv.TenantID)
	}
	hd.Set("Content-Length", strconv.Itoa(len(inv.Payload)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(inv.Payload); err != nil {
		h.log.Warn("runtime api: invocation not written to the runtime", "request_id", inv.ID, "error", err)
	}
}

// response serves POST /2018-06-01/runtime/invocation/{id}/response.
func (h *Handler) response(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, upload, ok := h.readResult(w, r, id)
	if !ok {
		return
	}
	defer upload.End()
	var err error
	if streamFailed(r) {
		errBody := streamedErrorBody(r)
		errorType := resolveErrorType(r.Trailer.Get(headerErrorType), errBody)
		err = h.machine.Fail(r.Context(), id, errorType, errorBody(errBody, errorType))
	} else {
		err = h.machine.Respond(r.Context(), id, body, r.Header.Get("Content-Type"))
	}
	h.accept(w, "response", id, err)
}

// invocationError serves POST /2018-06-01/runtime/invocation/{id}/error.
func (h *Handler) invocationError(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, upload, ok := h.readResult(w, r, id)
	if !ok {
		return
	}
	defer upload.End()
	errorType := resolveErrorType(r.Header.Get(headerErrorType), body)
	h.accept(w, "error", id, h.machine.Fail(r.Context(), id, errorType, errorBody(body, errorType)))
}

// initError serves POST /2018-06-01/runtime/init/error.
func (h *Handler) initError(w http.ResponseWriter, r *http.Request) {
	if err := h.machine.CheckInit(); err != nil {
		h.reject(w, "init/error", "", err)
		return
	}
	body, err := h.readBody(w, r)
	if err != nil {
		// An unreadable or oversized init error is still an init error;
		// only its document is lost.
		h.log.Warn("runtime api: init error body not read", "error", err)
		body = nil
	}
	errorType := resolveErrorType(r.Header.Get(headerErrorType), body)
	h.accept(w, "init/error", "", h.machine.InitFailed(r.Context(), errorType, errorBody(body, errorType)))
}

// readResult validates id before reading the body, then reads it within
// the payload limit; the upload is tracked until the caller ends it. An
// oversized body completes the invocation with OutcomeResponseTooLarge.
// It answers the runtime itself when !ok.
func (h *Handler) readResult(w http.ResponseWriter, r *http.Request, id string) ([]byte, *Upload, bool) {
	upload, err := h.machine.BeginResult(id)
	if err != nil {
		h.reject(w, r.URL.Path, id, err)
		return nil, nil, false
	}
	body, err := h.readBody(w, r)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		err = h.machine.TooLarge(r.Context(), id)
		upload.End()
		if err != nil {
			h.reject(w, r.URL.Path, id, err)
			return nil, nil, false
		}
		h.log.Info("runtime api: response too large", "request_id", truncate(id), "limit", h.maxPayload)
		writeError(w, errTooLarge(h.maxPayload))
		return nil, nil, false
	case err != nil:
		upload.End()
		h.log.Warn("runtime api: body not read", "request_id", truncate(id), "error", err)
		writeError(w, errBodyUnreadable)
		return nil, nil, false
	}
	return body, upload, true
}

// readBody reads the body within the payload limit, one request at a time.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	select {
	case h.reading <- struct{}{}:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	defer func() { <-h.reading }()
	return readLimited(w, r, h.maxPayload)
}

// readLimited reads the body, failing with *http.MaxBytesError beyond
// limit, without reading at all when Content-Length already exceeds it.
func readLimited(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, &http.MaxBytesError{Limit: limit}
	}
	return io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
}

func (h *Handler) accept(w http.ResponseWriter, op, id string, err error) {
	if err != nil {
		h.reject(w, op, id, err)
		return
	}
	h.log.Debug("runtime api: accepted", "op", op, "request_id", truncate(id))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"status":"OK"}`)
}

func (h *Handler) reject(w http.ResponseWriter, op, id string, err error) {
	op, id = truncate(op), truncate(id)
	var apiErr *apiError
	var lost *LostInvocationError
	switch {
	case errors.As(err, &lost):
		h.log.Warn("runtime api: invocation not handed to the runtime, its /next went away", "request_id", lost.ID)
		return
	case !errors.As(err, &apiErr):
		// The runtime went away while parked in /next: nobody to answer.
		h.log.Debug("runtime api: request abandoned", "op", op, "error", err)
		return
	}
	h.log.Warn("runtime api: request rejected", "op", op, "request_id", id, "status", apiErr.status, "error_type", apiErr.errorType)
	writeError(w, apiErr)
}

func writeError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_, _ = w.Write(errorDocument(e.message, e.errorType))
}

// errorDocument is the Lambda error shape: {"errorMessage", "errorType"}.
func errorDocument(message, errorType string) []byte {
	b, _ := json.Marshal(struct {
		ErrorMessage string `json:"errorMessage"`
		ErrorType    string `json:"errorType"`
	}{message, errorType})
	return b
}

// streamFailed reports whether a streamed response ended with an error
// trailer. A buffered response never has one.
func streamFailed(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get(headerResponseMode), "streaming") &&
		r.Trailer.Get(headerErrorType) != ""
}

// streamedErrorBody decodes the base64 error trailer of a streamed
// response; nil when it is missing or broken.
func streamedErrorBody(r *http.Request) []byte {
	body, err := base64.StdEncoding.DecodeString(r.Trailer.Get(trailerErrorBody))
	if err != nil {
		return nil
	}
	return body
}

// maxLoggedLen bounds the runtime-controlled strings written to our logs.
const maxLoggedLen = 128

func truncate(s string) string {
	if len(s) > maxLoggedLen {
		return s[:maxLoggedLen] + "…"
	}
	return s
}

// resolveErrorType returns the error type of a function or init error: the
// Lambda-Runtime-Function-Error-Type header when present (only /init/error
// sends it with the AWS clients), else the errorType of the JSON body, else
// Runtime.Unknown. The result is bounded in length.
func resolveErrorType(header string, body []byte) string {
	t := strings.TrimSpace(header)
	if t == "" {
		var doc struct {
			ErrorType string `json:"errorType"`
		}
		if json.Unmarshal(body, &doc) == nil {
			t = strings.TrimSpace(doc.ErrorType)
		}
	}
	if t = sanitizeErrorType(t); t == "" {
		return unknownErrorType
	}
	return t
}

// sanitizeErrorType keeps the printable ASCII characters of an error type,
// at most maxErrorTypeLen of them: it ends up in logs, metrics and headers.
func sanitizeErrorType(t string) string {
	var b strings.Builder
	for i := 0; i < len(t) && b.Len() < maxErrorTypeLen; i++ {
		if c := t[i]; c > ' ' && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// errorBody keeps the error document posted by the runtime, or builds one
// in the Lambda shape when it is empty.
func errorBody(body []byte, errorType string) []byte {
	if len(bytes.TrimSpace(body)) > 0 {
		return body
	}
	return errorDocument("Unknown application error occurred", errorType)
}
