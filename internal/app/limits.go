// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"syscall"

	"github.com/loch-sh/lambda-runtime/internal/config"
	"github.com/loch-sh/lambda-runtime/internal/queue"
)

// unverifiedLimits bound the requests not authenticated yet (SigV4 without
// a payload hash header): a quarter of the byte budget, at least one
// maximum body, and one maximum body per IP; a quarter of the queue size,
// at least one, and a quarter of that per IP (clients behind a NAT or a
// proxy share an IP).
func unverifiedLimits(rt config.Runtime) queue.UnverifiedLimits {
	const (
		share   = 4 // of the queue (bytes and count) left to unverified requests
		ipShare = 4 // of the unverified count left to one IP
	)
	count := max(1, rt.QueueSize/share)
	return queue.UnverifiedLimits{
		Bytes:      max(rt.QueueBytes/share, rt.MaxPayload),
		PerIP:      rt.MaxPayload,
		Count:      count,
		CountPerIP: max(1, count/ipShare),
	}
}

// connsPerIP caps the open Invoke API connections of one source IP
// (loopback exempt), idle keep-alive ones included, at a share of the
// global cap (at least 4, at most the global cap): with MaxConns ≥ 32, it
// takes 8 source addresses holding connections without sending their
// headers to fill the listener. As each request needs a connection, this
// also caps the requests in flight of one IP: with the default global cap
// (config.DefaultMaxConns), one IP can still fill the queue.
func connsPerIP(maxConns int) int {
	return min(maxConns, max(4, maxConns/config.ConnsPerIPShare))
}

// warnLowFileLimit warns when the runtime's open files limit leaves less
// than config.FileMargin besides maxConns: the connections beyond it would
// not be accepted (EMFILE). The limit is per process: the function has its
// own. It only warns: the panel sets the limit, and a smaller one still
// works.
func warnLowFileLimit(maxConns int, limit func() (uint64, error), log *slog.Logger) {
	n, err := limit()
	if err != nil {
		log.Warn("cannot read the open files limit", "error", err.Error())
		return
	}
	if want := uint64(maxConns + config.FileMargin); n < want {
		log.Warn("the open files limit is low for LOCH_RUNTIME_MAX_CONNS: lower it, or raise nofile",
			"nofile", n, "max_conns", maxConns, "want_nofile", want)
	}
}

// openFilesLimit returns the soft RLIMIT_NOFILE of the runtime (since Go
// 1.19, the Go runtime raises it to the hard limit at startup, and restores
// the original one for the child).
func openFilesLimit() (uint64, error) {
	var r syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r)
	return uint64(r.Cur), err
}
