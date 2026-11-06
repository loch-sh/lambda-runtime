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
	"os"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/auth"
	"github.com/loch-sh/lambda-runtime/internal/engine"
	"github.com/loch-sh/lambda-runtime/internal/queue"
)

// Authenticator checks the credential of a request before its body is
// read. It returns the error to answer, or nil and what is left to check
// once the body is read.
type Authenticator interface {
	Authenticate(r *http.Request) (auth.BodyCheck, error)
}

// Observer is told about the requests refused before reaching the engine.
type Observer interface {
	Throttled()
	AuthFailed()
}

// MetricsWriter writes the metrics in the Prometheus text format.
type MetricsWriter interface {
	WriteTo(w io.Writer) (int64, error)
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
	Auth Authenticator
	// LoopbackWithoutAuth exempts loopback callers from authentication:
	// inside the container's network namespace, only the function itself
	// and docker exec processes (invoke, health) can connect, and the
	// function gains nothing by invoking itself.
	LoopbackWithoutAuth bool
	// Observer and Metrics may be nil.
	Observer Observer
	Metrics  MetricsWriter
	Queue    *queue.Queue
	State    StateFunc
	Log      *slog.Logger
}

// Handler serves the Invoke API and /_loch/health.
type Handler struct {
	opts  Options
	mux   *http.ServeMux
	perIP *ipLimiter
	// authLog limits the logs of authentication failures.
	authLog *logLimiter
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
	// unverifiedBodyRate is the minimum upload rate of a request not
	// authenticated yet: 6 MiB within 5 s + 1.5 s, else 408.
	unverifiedBodyRate = 4 << 20
	// defaultMinBodyRate applies when Options.MinBodyRate is unset.
	defaultMinBodyRate = 1 << 20
)

// NewHandler returns the handler.
func NewHandler(opts Options) *Handler {
	if opts.MinBodyRate <= 0 {
		opts.MinBodyRate = defaultMinBodyRate
	}
	h := &Handler{opts: opts, mux: http.NewServeMux(), perIP: newIPLimiter(opts.MaxInFlightPerIP), authLog: newLogLimiter()}
	h.mux.HandleFunc("/2015-03-31/functions/{name}/invocations", h.invoke)
	h.mux.HandleFunc("/_loch/health", h.Health)
	h.mux.HandleFunc("/_loch/metrics", h.metrics)
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
	release, ok := h.admit(w, r, id)
	if !ok {
		return
	}
	defer release()
	bodyCheck, err := h.authenticate(r)
	if err != nil {
		h.fail(w, r, id, err)
		return
	}
	job, invType, err := h.parse(r, id)
	if err != nil {
		h.fail(w, r, id, err)
		return
	}
	if invType == typeDryRun {
		h.dryRun(w, r, id, bodyCheck)
		return
	}
	res, err := h.run(w, r, job, bodyCheck)
	if err != nil {
		h.fail(w, r, id, err)
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
func (h *Handler) run(w http.ResponseWriter, r *http.Request, job *engine.Job, bodyCheck auth.BodyCheck) (*engine.Result, error) {
	body, res, err := h.receive(w, r, bodyCheck)
	if err != nil {
		return nil, err
	}
	job.Payload = body
	job.Expires = time.Now().Add(h.opts.QueueTimeout)
	entry, err := res.Enqueue(job)
	if err != nil {
		return nil, errShuttingDown()
	}
	return h.wait(r.Context(), entry)
}

// receive reserves room in the queue, reads the body and completes the
// authentication with it. On success the reservation holds the body.
//
// A request whose authentication needs the body (SigV4 without a payload
// hash header) is not authenticated yet: its reservation comes from the
// smaller unverified budget, without a queue slot until it is verified,
// and its body must arrive faster.
func (h *Handler) receive(w http.ResponseWriter, r *http.Request, bodyCheck auth.BodyCheck) ([]byte, *queue.Reservation, error) {
	reserve := r.ContentLength
	if reserve < 0 {
		reserve = h.opts.MaxPayload
	}
	unverified := bodyCheck.Pending
	rate := h.opts.MinBodyRate
	var res *queue.Reservation
	var err error
	if unverified {
		res, err = h.opts.Queue.ReserveUnverified(reserve, remoteIP(r.RemoteAddr))
		rate = max(rate, unverifiedBodyRate)
	} else {
		res, err = h.opts.Queue.Reserve(reserve)
	}
	if err != nil {
		return nil, nil, h.queueError(err)
	}
	body, err := h.readBody(w, r, reserve, rate)
	if err == nil {
		err = bodyCheck.Check(body)
	}
	if err != nil {
		res.Release()
		return nil, nil, err
	}
	if err := res.Verified(); err != nil { // the reservation is released
		return nil, nil, h.queueError(err)
	}
	res.Shrink(int64(len(body)))
	return body, res, nil
}

// queueError is the answer to a refused reservation.
func (h *Handler) queueError(err error) error {
	if errors.Is(err, queue.ErrFull) {
		return h.throttled()
	}
	return errShuttingDown()
}

// dryRun answers a DryRun invocation: 204 once the request is fully
// authenticated (a SigV4 signature covers the body), nothing invoked.
func (h *Handler) dryRun(w http.ResponseWriter, r *http.Request, id string, bodyCheck auth.BodyCheck) {
	_, res, err := h.receive(w, r, bodyCheck)
	if err != nil {
		h.fail(w, r, id, err)
		return
	}
	res.Release()
	w.Header().Set("X-Amzn-RequestId", id)
	w.WriteHeader(http.StatusNoContent)
}

// readBody reads the body within the payload limit, into a buffer of the
// exact size when Content-Length is known (memory matches what the queue
// accounts). The upload must keep progressing (bodyProgress between
// reads) and end before a deadline that grows with its size, so that a
// slow sender cannot hold a queue reservation for long.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, size, rate int64) ([]byte, error) {
	rc := http.NewResponseController(w)
	body := &progressReader{
		r:    http.MaxBytesReader(w, r.Body, h.opts.MaxPayload),
		rc:   rc,
		hard: time.Now().Add(bodyGrace + time.Duration(size*int64(time.Second)/rate)),
	}
	var data []byte
	var err error
	if r.ContentLength >= 0 {
		data = make([]byte, r.ContentLength)
		_, err = io.ReadFull(body, data)
	} else {
		data, err = readChunked(body, h.opts.MaxPayload)
	}
	if err != nil {
		// Keep the deadline expired: fail closes the connection, and
		// net/http must not wait for the rest of the body meanwhile.
		_ = rc.SetReadDeadline(time.Now())
	} else {
		_ = rc.SetReadDeadline(time.Time{})
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, errTooLarge(h.opts.MaxPayload)
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil, errBodyTimeout()
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
	if h.opts.Observer != nil {
		h.opts.Observer.Throttled()
	}
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

// admit takes a slot of the per-IP cap of requests in flight, or answers
// 429 (and closes the connection) when the source IP is at its cap.
func (h *Handler) admit(w http.ResponseWriter, r *http.Request, id string) (release func(), ok bool) {
	release, ok = h.perIP.acquire(r.RemoteAddr)
	if !ok {
		w.Header().Set("Connection", "close")
		h.fail(w, r, id, h.throttled())
	}
	return release, ok
}

// authenticate checks the credential; nothing when authentication is off
// or for an exempt loopback caller (Options.LoopbackWithoutAuth).
func (h *Handler) authenticate(r *http.Request) (auth.BodyCheck, error) {
	if h.opts.Auth == nil || (h.opts.LoopbackWithoutAuth && isLoopback(r.RemoteAddr)) {
		return auth.BodyCheck{}, nil
	}
	return h.opts.Auth.Authenticate(r)
}

// fail answers err in the AWS shape. An authentication failure also
// closes the connection, is counted, and is logged at most once a minute
// per source IP.
//
// A request with a body closes the connection too: the body may be unread,
// and net/http would wait for it before answering, with no deadline.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, id string, err error) {
	if r.ContentLength != 0 {
		w.Header().Set("Connection", "close")
	}
	aerr, isAuth := asAWSError(err)
	switch {
	case isAuth:
		w.Header().Set("Connection", "close")
		if h.opts.Observer != nil {
			h.opts.Observer.AuthFailed()
		}
		if h.authLog.allow(r.RemoteAddr) {
			h.opts.Log.Warn("invoke: authentication failed", "remote", r.RemoteAddr, "error_type", aerr.name)
		}
	case aerr == nil:
		aerr = &awsError{status: http.StatusInternalServerError, name: "ServiceException", message: "Internal error"}
		h.opts.Log.Error("invoke: unexpected error", "request_id", id, "error", err)
	case aerr.status == http.StatusTooManyRequests || aerr.status >= 500:
		h.opts.Log.Info("invoke: refused", "request_id", id, "error_type", aerr.name)
	}
	aerr.write(w, id)
}

// asAWSError converts err to the AWS error to answer, telling whether it
// is an authentication failure; nil for an unexpected error.
func asAWSError(err error) (*awsError, bool) {
	var authErr *auth.Error
	if errors.As(err, &authErr) {
		return &awsError{status: authErr.Status, name: authErr.Name, message: authErr.Message}, true
	}
	var aerr *awsError
	if errors.As(err, &aerr) {
		return aerr, false
	}
	return nil, false
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

// metrics serves GET /_loch/metrics, behind the same credential and
// per-IP cap as the Invoke API.
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	id := requestID()
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		errMethod().write(w, id)
		return
	}
	release, ok := h.admit(w, r, id)
	if !ok {
		return
	}
	defer release()
	check, err := h.authenticate(r)
	if err == nil {
		err = check.Check(nil) // a GET has no body
	}
	if err != nil {
		h.fail(w, r, id, err)
		return
	}
	if h.opts.Metrics == nil {
		errRouteNotFound().write(w, id)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = h.opts.Metrics.WriteTo(w)
}
