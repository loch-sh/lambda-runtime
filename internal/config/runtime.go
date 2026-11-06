// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults of the runtime settings.
const (
	DefaultInvokeAddr     = "0.0.0.0:8080"
	DefaultRuntimeAPIAddr = "127.0.0.1:9001"
	DefaultHealthAddr     = "127.0.0.1:8081"
	DefaultBootstrap      = "/lambda-entrypoint.sh"
	DefaultInitTimeout    = 10 * time.Second
	DefaultShutdownGrace  = 8 * time.Second
	DefaultMaxPayload     = 6 << 20
	// responseAllowance is added to the request limit for responses, as
	// Lambda does: 6 MiB requests, 6,291,556-byte responses.
	responseAllowance   = 100
	DefaultQueueSize    = 16
	DefaultQueueTimeout = 30 * time.Second
	DefaultLogLevel     = "info"
	// defaultQueueBytesCap is the upper bound of the default byte budget of
	// the queue; the default is min(32 MiB, memory/8), never below one
	// maximum payload.
	defaultQueueBytesCap = 32 << 20

	// ConnsPerIPShare: one source IP may hold 1/ConnsPerIPShare of the
	// Invoke API connections (internal/app, limits.go). The default
	// connection cap is ConnsPerIPShare × (queue size + 1), so that one IP
	// can fill the queue.
	ConnsPerIPShare = 8
	// FileMargin is the room left, under the open files limit, for the
	// runtime's other descriptors (Runtime API, health, pipes, logs).
	FileMargin = 256
	// defaultMaxConnsCap bounds the default connection cap, under the usual
	// 1024 open files.
	defaultMaxConnsCap = 1024 - FileMargin

	maxQueueSize    = 100000
	maxConns        = 100000
	minPayloadLimit = 1 << 10
	maxPayloadLimit = 1 << 30
	maxQueueBytes   = 64 << 30
	// maxSettingDuration bounds the LOCH_RUNTIME_* durations.
	maxSettingDuration = 24 * time.Hour
)

// Runtime holds the LOCH_RUNTIME_* settings.
type Runtime struct {
	InvokeAddr     string
	RuntimeAPIAddr string
	// HealthAddr serves /_loch/health alone, on the loopback, so that the
	// healthcheck never competes with Invoke API clients for connections.
	HealthAddr string
	Bootstrap  string
	// Workdir is the working directory of the child; "" keeps ours.
	Workdir       string
	InitTimeout   time.Duration
	ShutdownGrace time.Duration
	// MaxPayload bounds the invocation requests.
	MaxPayload   int64
	QueueSize    int
	QueueBytes   int64
	QueueTimeout time.Duration
	MaxConns     int
	LogLevel     string
	Auth         Auth
}

// MaxResponse bounds the responses and errors posted by the runtime.
func (r Runtime) MaxResponse() int64 { return r.MaxPayload + responseAllowance }

// parseRuntime validates the raw LOCH_RUNTIME_* values. memoryMB sizes the
// default byte budget of the queue (0 when unknown).
func parseRuntime(v rawValues, env Env, memoryMB int) (Runtime, []error) {
	p := &parser{values: v}
	rt := Runtime{
		InvokeAddr:     p.addr("LOCH_RUNTIME_INVOKE_ADDR", DefaultInvokeAddr, false),
		RuntimeAPIAddr: p.addr("LOCH_RUNTIME_API_ADDR", DefaultRuntimeAPIAddr, true),
		HealthAddr:     p.addr("LOCH_RUNTIME_HEALTH_ADDR", DefaultHealthAddr, true),
		Bootstrap:      p.absPathOr("LOCH_RUNTIME_BOOTSTRAP", DefaultBootstrap),
		Workdir:        p.absPath("LOCH_RUNTIME_WORKDIR"),
		InitTimeout:    p.duration("LOCH_RUNTIME_INIT_TIMEOUT", DefaultInitTimeout, false),
		ShutdownGrace:  p.duration("LOCH_RUNTIME_SHUTDOWN_GRACE", DefaultShutdownGrace, true),
		QueueTimeout:   p.duration("LOCH_RUNTIME_QUEUE_TIMEOUT", DefaultQueueTimeout, false),
		MaxPayload:     p.size("LOCH_RUNTIME_MAX_PAYLOAD", DefaultMaxPayload, minPayloadLimit, maxPayloadLimit),
		QueueSize:      p.integer("LOCH_RUNTIME_QUEUE_SIZE", DefaultQueueSize, 0, maxQueueSize),
		LogLevel:       p.logLevel("LOCH_RUNTIME_LOG_LEVEL"),
		Auth: Auth{
			Enabled:         p.onOff("LOCH_RUNTIME_AUTH", true),
			Token:           secretFrom(env, envToken),
			TokenFile:       p.absPath("LOCH_RUNTIME_TOKEN_FILE"),
			TokenSHA256:     lookupTrimmed(env, envTokenSHA256),
			TokenSHA256File: p.absPath("LOCH_RUNTIME_TOKEN_SHA256_FILE"),
			AccessKey:       secretFrom(env, envAccessKey),
			AccessKeyFile:   p.absPath("LOCH_RUNTIME_ACCESS_KEY_FILE"),
		},
	}
	// These defaults depend on the values parsed above.
	rt.MaxConns = p.integer("LOCH_RUNTIME_MAX_CONNS", DefaultMaxConns(rt.QueueSize), 1, maxConns)
	rt.QueueBytes = p.size("LOCH_RUNTIME_QUEUE_BYTES", defaultQueueBytes(rt.MaxPayload, memoryMB), 0, maxQueueBytes)
	if rt.QueueBytes < rt.MaxPayload && v["LOCH_RUNTIME_QUEUE_BYTES"] != "" {
		p.fail("LOCH_RUNTIME_QUEUE_BYTES", "%d is below LOCH_RUNTIME_MAX_PAYLOAD (%d)", rt.QueueBytes, rt.MaxPayload)
	}
	return rt, p.errs
}

func secretFrom(env Env, key string) Secret { return Secret(lookupTrimmed(env, key)) }

func lookupTrimmed(env Env, key string) string {
	v, _ := env.Lookup(key)
	return strings.TrimSpace(v)
}

// DefaultMaxConns is the default cap of open Invoke API connections:
// ConnsPerIPShare × (queueSize + 1), so that one source IP can fill the
// queue, at most defaultMaxConnsCap (768).
func DefaultMaxConns(queueSize int) int {
	return min(ConnsPerIPShare*(queueSize+1), defaultMaxConnsCap)
}

// defaultQueueBytes is min(32 MiB, memory/8), raised to one maximum payload
// so that the largest accepted request can always be queued.
func defaultQueueBytes(maxPayload int64, memoryMB int) int64 {
	n := int64(defaultQueueBytesCap)
	if memoryMB > 0 {
		n = min(n, int64(memoryMB)<<20/8)
	}
	return max(n, maxPayload)
}

// parser accumulates validation errors while reading raw values.
type parser struct {
	values rawValues
	errs   []error
}

func (p *parser) fail(key string, format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(key+": "+format, args...))
}

func (p *parser) addr(key, def string, loopbackOnly bool) string {
	v := orDefault(p.values[key], def)
	if err := validateAddr(v, loopbackOnly); err != nil {
		p.fail(key, "%v", err)
	}
	return v
}

// duration reads a Go duration or a number of seconds. Zero is accepted
// only when allowZero is set.
func (p *parser) duration(key string, def time.Duration, allowZero bool) time.Duration {
	v := p.values[key]
	if v == "" {
		return def
	}
	d, err := parseDuration(v, maxSettingDuration)
	switch {
	case err != nil:
		p.fail(key, "%v", err)
	case d == 0 && !allowZero:
		p.fail(key, "must be positive")
	default:
		return d
	}
	return def
}

func (p *parser) size(key string, def, lo, hi int64) int64 {
	v := p.values[key]
	if v == "" {
		return def
	}
	n, err := parseSize(v)
	switch {
	case err != nil:
		p.fail(key, "%v", err)
	case n < lo || n > hi:
		p.fail(key, "%d is outside [%d, %d]", n, lo, hi)
	default:
		return n
	}
	return def
}

func (p *parser) integer(key string, def, lo, hi int) int {
	v := p.values[key]
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	switch {
	case err != nil:
		p.fail(key, "%q is not an integer", v)
	case n < lo || n > hi:
		p.fail(key, "%d is outside [%d, %d]", n, lo, hi)
	default:
		return n
	}
	return def
}

func (p *parser) onOff(key string, def bool) bool {
	switch strings.ToLower(p.values[key]) {
	case "":
		return def
	case "on":
		return true
	case "off":
		return false
	}
	p.fail(key, "%q is neither on nor off", p.values[key])
	return def
}

// absPath reads an optional absolute path ("" when unset).
func (p *parser) absPath(key string) string {
	return p.absPathOr(key, "")
}

func (p *parser) absPathOr(key, def string) string {
	v := orDefault(p.values[key], def)
	if v != "" && !filepath.IsAbs(v) {
		p.fail(key, "%q must be an absolute path", v)
	}
	return v
}

func (p *parser) logLevel(key string) string {
	v := strings.ToLower(orDefault(p.values[key], DefaultLogLevel))
	switch v {
	case "debug", "info", "warn", "error":
		return v
	}
	p.fail(key, "%q is not one of debug, info, warn, error", v)
	return DefaultLogLevel
}
