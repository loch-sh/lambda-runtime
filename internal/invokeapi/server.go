// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package invokeapi

import (
	"log/slog"
	"net/http"
	"time"
)

// Limits of the Invoke API server (anti-slowloris). There is no write
// timeout: a response waits for the function, up to its timeout.
const (
	readHeaderTimeout = 5 * time.Second
	// idleTimeout is short: an idle keep-alive connection holds a slot of
	// the connection cap.
	idleTimeout    = 5 * time.Second
	maxHeaderBytes = 32 << 10
)

// NewServer returns the HTTP server of the Invoke API (or of the health
// listener). Its listener must cap the number of connections
// (netutil.LimitListener, netutil.PerIPLimitListener).
func NewServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// ThrottledResponse is written on the connections refused before any
// request is read (per-IP cap): an AWS-shaped 429 that SDKs retry.
var ThrottledResponse = []byte("HTTP/1.1 429 Too Many Requests\r\n" +
	"Content-Type: application/json\r\n" +
	"X-Amzn-ErrorType: TooManyRequestsException\r\n" +
	"Retry-After: 1\r\n" +
	"Connection: close\r\n" +
	"Content-Length: 66\r\n\r\n" +
	`{"Type":"User","message":"Too many connections from this address"}`)
