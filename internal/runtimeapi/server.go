// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package runtimeapi

import (
	"log/slog"
	"net/http"
)

// maxHeaderBytes leaves room for the X-Ray error cause header, which the
// Python client caps just below 1 MiB and the Node.js client not at all.
const maxHeaderBytes = 4 << 20

// NewServer returns the HTTP server of the Runtime API. It sets no read,
// write or idle timeout: /next is a long poll that may wait for hours, and
// AWS tells runtimes not to time it out. It is meant for a loopback
// listener only.
func NewServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:        h,
		MaxHeaderBytes: maxHeaderBytes,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}
