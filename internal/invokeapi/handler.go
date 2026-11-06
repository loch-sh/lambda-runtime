// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package invokeapi serves the AWS Lambda Invoke API
// (POST /2015-03-31/functions/{name}/invocations) and the health endpoint.
package invokeapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/engine"
	"github.com/loch-sh/lambda-runtime/internal/queue"
)

// Authenticator checks the credential of a request before its body is
// read. It returns nil when the request may proceed, else the error to
// answer.
type Authenticator interface {
	Authenticate(r *http.Request) error
}

// StateFunc returns the state of the runtime, for the health endpoint.
type StateFunc func() engine.State

// Options configures the Handler.
type Options struct {
	Function     FunctionName
	Version      string
	MaxPayload   int64
	QueueTimeout time.Duration
	// MinBodyRate is the slowest accepted upload, in bytes per second.
	MinBodyRate int64
	// MaxInFlightPerIP caps the requests in flight per source IP (loopback
	// exempt); 0 disables the cap.
	MaxInFlightPerIP int
	// RetryAfter returns the delay to advertise in Retry-After, in seconds;
	// nil means 1.
	RetryAfter func() int
	// Auth is nil when authentication is off.
	Auth  Authenticator
	Queue *queue.Queue
	State StateFunc
	Log   *slog.Logger
}

// Handler serves the Invoke API and /_loch/health.
type Handler struct {
	opts  Options
	mux   *http.ServeMux
	perIP *ipLimiter
}

// errQueueTimeout cancels the wait of a job that stayed queued too long.
var errQueueTimeout = errors.New("queue timeout")

const (
	// bodyGrace is the base read deadline of a body, before the size-based
	// part given by MinBodyRate.
	bodyGrace = 5 * time.Second
	// bodyProgress is the longest pause accepted while a body is read.
	bodyProgress = 5 * time.Second
	// maxRetryAfter bounds the advertised Retry-After, in seconds.
	maxRetryAfter = 30
	// defaultMinBodyRate applies when Options.MinBodyRate is unset.
	defaultMinBodyRate = 1 << 20
)

// NewHandler returns the handler.
func NewHandler(opts Options) *Handler {
	if opts.MinBodyRate <= 0 {
		opts.MinBodyRate = defaultMinBodyRate
	}
	h := &Handler{opts: opts, mux: http.NewServeMux(), perIP: newIPLimiter(opts.MaxInFlightPerIP)}
	h.mux.HandleFunc("/2015-03-31/functions/{name}/invocations", h.invoke)
	h.mux.HandleFunc("/_loch/health", h.Health)
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		errRouteNotFound().write(w, requestID())
	})
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// invoke serves POST /2015-03-31/functions/{name}/invocations.
func (h *Handler) invoke(w http.ResponseWriter, r *http.Request) {
	id := requestID()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		errMethod().write(w, id)
		return
	}
	release, ok := h.perIP.acquire(r.RemoteAddr)
	if !ok {
		w.Header().Set("Connection", "close")
		h.writeErr(w, id, h.throttled())
		return
	}
	defer release()
	if h.opts.Auth != nil {
		if err := h.opts.Auth.Authenticate(r); err != nil {
			h.writeErr(w, id, err)
			return
		}
	}
	job, invType, err := h.parse(r, id)
	if err != nil {
		h.writeErr(w, id, err)
		return
	}
	if invType == typeDryRun {
		w.Header().Set("X-Amzn-RequestId", id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	res, err := h.run(w, r, job)
	if err != nil {
		h.writeErr(w, id, err)
		return
	}
	if res != nil {
		h.writeResult(w, id, res, wantsTail(r))
	}
}

// parse validates everything but the body and builds the job.
func (h *Handler) parse(r *http.Request, id string) (*engine.Job, string, error) {
	if !h.opts.Function.matches(r.PathValue("name")) {
		return nil, "", errNotFound(r.PathValue("name"))
	}
	if q := r.URL.Query().Get("Qualifier"); q != "" && q != latest {
		return nil, "", errNotFound(r.PathValue("name") + ":" + q)
	}
	invType, aerr := invocationType(r)
	if aerr != nil {
		return nil, "", aerr
	}
	cc, aerr := clientContext(r)
	if aerr != nil {
		return nil, "", aerr
	}
	if r.ContentLength > h.opts.MaxPayload {
		return nil, "", errTooLarge(h.opts.MaxPayload)
	}
	job := engine.NewJob(engine.Request{
		RequestID:     id,
		TraceID:       traceID(r, time.Now()),
		ClientContext: cc,
		TenantID:      tenantID(r),
	})
	return job, invType, nil
}

// run admits, reads and queues the request, then waits for its result. A
// nil result with a nil error means the client went away.
func (h *Handler) run(w http.ResponseWriter, r *http.Request, job *engine.Job) (*engine.Result, error) {
	reserve := r.ContentLength
	if reserve < 0 {
		reserve = h.opts.MaxPayload
	}
	res, err := h.opts.Queue.Reserve(reserve)
	switch {
	case errors.Is(err, queue.ErrFull):
		return nil, h.throttled()
	case err != nil:
		return nil, errShuttingDown()
	}
	body, err := h.readBody(w, r, reserve)
	if err != nil {
		res.Release()
		return nil, err
	}
	res.Shrink(int64(len(body)))
	job.Payload = body
	job.Expires = time.Now().Add(h.opts.QueueTimeout)
	entry, err := res.Enqueue(job)
	if err != nil {
		return nil, errShuttingDown()
	}
	return h.wait(r.Context(), entry)
}

// readBody reads the body within the payload limit, into a buffer of the
// exact size when Content-Length is known (memory matches what the queue
// accounts). The upload must keep progressing (bodyProgress between
// reads) and end before a deadline that grows with its size, so that a
// slow sender cannot hold a queue reservation for long.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, size int64) ([]byte, error) {
	rc := http.NewResponseController(w)
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	body := &progressReader{
		r:    http.MaxBytesReader(w, r.Body, h.opts.MaxPayload),
		rc:   rc,
		hard: time.Now().Add(bodyGrace + time.Duration(size*int64(time.Second)/h.opts.MinBodyRate)),
	}
	var data []byte
	var err error
	if r.ContentLength >= 0 {
		data = make([]byte, r.ContentLength)
		_, err = io.ReadFull(body, data)
	} else {
		data, err = readChunked(body, h.opts.MaxPayload)
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, errTooLarge(h.opts.MaxPayload)
	case err != nil:
		return nil, errInvalidContent("The request body could not be read")
	}
	return data, nil
}

// readChunked reads a body of unknown size into one buffer of the
// reserved size (MaxPayload): no growth by doubling. A small body is
// copied to a buffer of its size so that the big one is freed at once.
func readChunked(r io.Reader, max int64) ([]byte, error) {
	buf := make([]byte, max+1)
	n, err := io.ReadFull(r, buf)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		err = nil
	case err == nil:
		err = &http.MaxBytesError{Limit: max} // max+1 bytes read
	}
	if err != nil {
		return nil, err
	}
	if n < len(buf)/2 {
		return bytes.Clone(buf[:n]), nil
	}
	return buf[:n:n], nil
}

// progressReader re-arms the read deadline before each read: the next
// bytes must come within bodyProgress, and all of them before hard.
type progressReader struct {
	r    io.Reader
	rc   *http.ResponseController
	hard time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	deadline := time.Now().Add(bodyProgress)
	if deadline.After(p.hard) {
		deadline = p.hard
	}
	_ = p.rc.SetReadDeadline(deadline)
	return p.r.Read(b)
}

// wait waits for the result. A job still queued after QueueTimeout is
// withdrawn (429); one already popped but not started is expired by the
// engine at the same deadline (Request.Expires, 429 too). A job whose
// client went away is withdrawn if still queued, else runs to completion
// and its result is dropped.
func (h *Handler) wait(ctx context.Context, entry *queue.Entry) (*engine.Result, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(h.opts.QueueTimeout, func() {
		if h.opts.Queue.Cancel(entry) {
			cancel(errQueueTimeout)
		}
	})
	defer timer.Stop()
	res, err := entry.Job.Wait(ctx)
	switch {
	case err == nil:
		return res, nil
	case errors.Is(context.Cause(ctx), errQueueTimeout), errors.Is(err, engine.ErrQueueTimeout):
		return nil, h.throttled()
	case errors.Is(err, engine.ErrShuttingDown):
		return nil, errShuttingDown()
	default:
		h.opts.Queue.Cancel(entry)
		return nil, nil
	}
}

// throttled is the 429 answer, with the Retry-After the runtime advises
// (the time left before a re-init, for instance).
func (h *Handler) throttled() *awsError {
	retry := 1
	if h.opts.RetryAfter != nil {
		retry = min(max(h.opts.RetryAfter(), 1), maxRetryAfter)
	}
	return errThrottled(retry)
}

func (h *Handler) writeResult(w http.ResponseWriter, id string, res *engine.Result, tail bool) {
	hd := w.Header()
	hd.Set("Content-Type", safeContentType(res.ContentType))
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Amzn-RequestId", id)
	hd.Set("X-Amz-Executed-Version", h.opts.Version)
	if res.FunctionError() {
		hd.Set("X-Amz-Function-Error", "Unhandled")
	}
	if tail && len(res.LogTail) > 0 {
		hd.Set("X-Amz-Log-Result", base64.StdEncoding.EncodeToString(res.LogTail))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Payload)
}

func (h *Handler) writeErr(w http.ResponseWriter, id string, err error) {
	var aerr *awsError
	if !errors.As(err, &aerr) {
		aerr = &awsError{status: http.StatusInternalServerError, name: "ServiceException", message: "Internal error"}
		h.opts.Log.Error("invoke: unexpected error", "request_id", id, "error", err)
	}
	if aerr.status == http.StatusTooManyRequests || aerr.status >= 500 {
		h.opts.Log.Info("invoke: refused", "request_id", id, "error_type", aerr.name)
	}
	aerr.write(w, id)
}

// Health serves GET /_loch/health; it never invokes the function and needs
// no credential. It is also served alone on the health listener.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	state := h.opts.State()
	stats := h.opts.Queue.Stats()
	status := http.StatusOK
	if state != engine.StateReady {
		status = http.StatusServiceUnavailable
	}
	b, _ := json.Marshal(struct {
		Status     engine.State `json:"status"`
		QueueDepth int          `json:"queueDepth"`
	}{state, stats.Depth})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
