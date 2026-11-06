// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package ids generates and validates the identifiers of an invocation:
// the request id (a UUID) and the X-Ray trace id.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"time"
)

// NewRequestID returns a random UUID v4, the shape of Lambda request ids.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// NewTraceID returns an X-Ray trace header for an unsampled request:
// Root=1-<8 hex epoch seconds>-<24 hex>;Parent=<16 hex>;Sampled=0.
func NewTraceID(now time.Time) string {
	var b [20]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	epoch := strconv.FormatInt(now.Unix(), 16)
	for len(epoch) < 8 {
		epoch = "0" + epoch
	}
	return "Root=1-" + epoch + "-" + h[:24] + ";Parent=" + h[24:40] + ";Sampled=0"
}

var tracePattern = regexp.MustCompile(`^Root=1-[0-9a-f]{8}-[0-9a-f]{24}(;Parent=[0-9a-f]{16})?(;Sampled=[01?])?(;Lineage=[0-9a-zA-Z:]{1,64})?$`)

// ValidTraceID reports whether s is a well-formed X-Ray trace header, as a
// caller may pass in X-Amzn-Trace-Id.
func ValidTraceID(s string) bool { return len(s) <= 256 && tracePattern.MatchString(s) }
