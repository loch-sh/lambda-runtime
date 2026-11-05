// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults of the runtime settings.
const (
	DefaultInvokeAddr     = "0.0.0.0:8080"
	DefaultRuntimeAPIAddr = "127.0.0.1:9001"
	DefaultBootstrap      = "/lambda-entrypoint.sh"
	DefaultInitTimeout    = 10 * time.Second
	DefaultShutdownGrace  = 8 * time.Second
	DefaultMaxPayload     = 6 << 20
	// responseAllowance is added to the request limit for responses, as
	// Lambda does: 6 MiB requests, 6,291,556-byte responses.
	responseAllowance   = 100
	DefaultQueueSize    = 16
	DefaultQueueTimeout = 30 * time.Second
	DefaultMaxConns     = 64
	DefaultLogLevel     = "info"
	// defaultQueueBytesCap is the upper bound of the default byte budget of
	// the queue; the default is min(32 MiB, memory/8), never below one
	// maximum payload.
	defaultQueueBytesCap = 32 << 20

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
	Bootstrap      string
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

// Auth configures the authentication of the Invoke API. Each credential
// comes either from an environment variable (read once, then removed from
// the environment) or from a file, never both.
type Auth struct {
	// Enabled is false only with LOCH_RUNTIME_AUTH=off.
	Enabled bool
	// Token is the bearer token given by LOCH_RUNTIME_TOKEN.
	Token Secret
	// TokenFile holds the bearer token.
	TokenFile string
	// AccessKey is "<access key id>:<secret>" given by LOCH_RUNTIME_ACCESS_KEY.
	AccessKey Secret
	// AccessKeyFile holds "<access key id>:<secret>" for SigV4.
	AccessKeyFile string
}

// Environment variables carrying credentials. They have no flag: a flag
// would show in the process list.
const (
	envToken     = "LOCH_RUNTIME_TOKEN"
	envAccessKey = "LOCH_RUNTIME_ACCESS_KEY"
	// MinTokenLen is the minimum length of a bearer token.
	MinTokenLen = 32
)

// SecretEnv lists the variables to remove from our environment once read.
var SecretEnv = []string{envToken, envAccessKey}

// Secret is a credential; it never prints its value.
type Secret string

// String implements fmt.Stringer without revealing the value.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

// GoString implements fmt.GoStringer (%#v) without revealing the value.
func (s Secret) GoString() string { return strconv.Quote(s.String()) }

// LogValue implements slog.LogValuer without revealing the value.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalJSON implements json.Marshaler without revealing the value.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(s.String())), nil }

// validate checks the credentials: one source per credential, a
// well-formed value, and at least one credential when authentication is on.
func (a Auth) validate() []error {
	var errs []error
	if a.Token != "" && a.TokenFile != "" {
		errs = append(errs, errors.New(envToken+" and LOCH_RUNTIME_TOKEN_FILE are both set: choose one"))
	}
	if a.AccessKey != "" && a.AccessKeyFile != "" {
		errs = append(errs, errors.New(envAccessKey+" and LOCH_RUNTIME_ACCESS_KEY_FILE are both set: choose one"))
	}
	if a.Token != "" {
		errs = appendIf(errs, ValidateToken(string(a.Token)))
	}
	if a.AccessKey != "" {
		_, _, err := ParseAccessKey(string(a.AccessKey))
		errs = appendIf(errs, err)
	}
	if a.Enabled && a.Token == "" && a.TokenFile == "" && a.AccessKey == "" && a.AccessKeyFile == "" {
		errs = append(errs, errors.New("authentication is on: set LOCH_RUNTIME_TOKEN(_FILE) and/or LOCH_RUNTIME_ACCESS_KEY(_FILE), or LOCH_RUNTIME_AUTH=off"))
	}
	return errs
}

// ValidateToken checks a bearer token without revealing it in the error.
func ValidateToken(token string) error {
	if len(token) < MinTokenLen {
		return fmt.Errorf("the bearer token must be at least %d bytes long", MinTokenLen)
	}
	for i := 0; i < len(token); i++ {
		if c := token[i]; c <= ' ' || c >= 0x7f {
			return errors.New("the bearer token must be printable ASCII without spaces")
		}
	}
	return nil
}

// ParseAccessKey splits "<access key id>:<secret>" without revealing the
// secret in the error.
func ParseAccessKey(v string) (id, secret string, err error) {
	id, secret, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok || id == "" || secret == "" || strings.ContainsAny(id, " \t\n") || strings.ContainsAny(secret, " \t\n") {
		return "", "", errors.New("the access key must be <access key id>:<secret>")
	}
	return id, secret, nil
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
		Bootstrap:      p.absPathOr("LOCH_RUNTIME_BOOTSTRAP", DefaultBootstrap),
		Workdir:        p.absPath("LOCH_RUNTIME_WORKDIR"),
		InitTimeout:    p.duration("LOCH_RUNTIME_INIT_TIMEOUT", DefaultInitTimeout, false),
		ShutdownGrace:  p.duration("LOCH_RUNTIME_SHUTDOWN_GRACE", DefaultShutdownGrace, true),
		QueueTimeout:   p.duration("LOCH_RUNTIME_QUEUE_TIMEOUT", DefaultQueueTimeout, false),
		MaxPayload:     p.size("LOCH_RUNTIME_MAX_PAYLOAD", DefaultMaxPayload, minPayloadLimit, maxPayloadLimit),
		QueueSize:      p.integer("LOCH_RUNTIME_QUEUE_SIZE", DefaultQueueSize, 0, maxQueueSize),
		MaxConns:       p.integer("LOCH_RUNTIME_MAX_CONNS", DefaultMaxConns, 1, maxConns),
		LogLevel:       p.logLevel("LOCH_RUNTIME_LOG_LEVEL"),
		Auth: Auth{
			Enabled:       p.onOff("LOCH_RUNTIME_AUTH", true),
			Token:         secretFrom(env, envToken),
			TokenFile:     p.absPath("LOCH_RUNTIME_TOKEN_FILE"),
			AccessKey:     secretFrom(env, envAccessKey),
			AccessKeyFile: p.absPath("LOCH_RUNTIME_ACCESS_KEY_FILE"),
		},
	}
	rt.QueueBytes = p.size("LOCH_RUNTIME_QUEUE_BYTES", defaultQueueBytes(rt.MaxPayload, memoryMB), 0, maxQueueBytes)
	if rt.QueueBytes < rt.MaxPayload && v["LOCH_RUNTIME_QUEUE_BYTES"] != "" {
		p.fail("LOCH_RUNTIME_QUEUE_BYTES", "%d is below LOCH_RUNTIME_MAX_PAYLOAD (%d)", rt.QueueBytes, rt.MaxPayload)
	}
	return rt, p.errs
}

func secretFrom(env Env, key string) Secret {
	v, _ := env.Lookup(key)
	return Secret(strings.TrimSpace(v))
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
