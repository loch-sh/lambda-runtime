// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// Auth configures the authentication of the Invoke API.
//
// The bearer token comes from exactly one source. The recommended one is
// its SHA-256 (LOCH_RUNTIME_TOKEN_SHA256): the token itself is then never in
// the container, where the function's code could read it (processes started
// with docker exec inherit the container environment). A plain token
// (LOCH_RUNTIME_TOKEN, or a file) is accepted for standalone use.
//
// The SigV4 key pair is optional; HMAC needs the secret itself, so the
// function's code can always read it: it only lets the function invoke
// itself.
type Auth struct {
	// Enabled is false only with LOCH_RUNTIME_AUTH=off.
	Enabled bool
	// Token is the bearer token given by LOCH_RUNTIME_TOKEN.
	Token Secret
	// TokenFile holds the bearer token.
	TokenFile string
	// TokenSHA256 is the hex SHA-256 of the bearer token.
	TokenSHA256 string
	// TokenSHA256File holds the hex SHA-256 of the bearer token.
	TokenSHA256File string
	// AccessKey is "<access key id>:<secret>" given by LOCH_RUNTIME_ACCESS_KEY.
	AccessKey Secret
	// AccessKeyFile holds "<access key id>:<secret>" for SigV4.
	AccessKeyFile string
}

// Environment variables carrying credentials. They have no flag: a flag
// would show in the process list.
const (
	envToken       = "LOCH_RUNTIME_TOKEN"
	envTokenSHA256 = "LOCH_RUNTIME_TOKEN_SHA256"
	envAccessKey   = "LOCH_RUNTIME_ACCESS_KEY"
	// MinTokenLen is the minimum length of a bearer token.
	MinTokenLen = 32
)

// SecretEnv lists the variables to remove from our environment once read.
var SecretEnv = []string{envToken, envTokenSHA256, envAccessKey}

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

// bearerSources counts the configured sources of the bearer token.
func (a Auth) bearerSources() int {
	n := 0
	for _, set := range []bool{a.Token != "", a.TokenFile != "", a.TokenSHA256 != "", a.TokenSHA256File != ""} {
		if set {
			n++
		}
	}
	return n
}

// HasBearer reports whether a bearer token is configured.
func (a Auth) HasBearer() bool { return a.bearerSources() > 0 }

// HasAccessKey reports whether a SigV4 key pair is configured.
func (a Auth) HasAccessKey() bool { return a.AccessKey != "" || a.AccessKeyFile != "" }

// validate checks the credentials: one source per credential, a
// well-formed value, and at least one credential when authentication is on.
func (a Auth) validate() []error {
	var errs []error
	if a.bearerSources() > 1 {
		errs = append(errs, errors.New("the bearer token has several sources: set one of LOCH_RUNTIME_TOKEN, "+
			"LOCH_RUNTIME_TOKEN_FILE, LOCH_RUNTIME_TOKEN_SHA256, LOCH_RUNTIME_TOKEN_SHA256_FILE"))
	}
	if a.AccessKey != "" && a.AccessKeyFile != "" {
		errs = append(errs, errors.New(envAccessKey+" and LOCH_RUNTIME_ACCESS_KEY_FILE are both set: choose one"))
	}
	if a.Token != "" {
		errs = appendIf(errs, ValidateToken(string(a.Token)))
	}
	if a.TokenSHA256 != "" {
		_, err := ParseTokenSHA256(a.TokenSHA256)
		errs = appendIf(errs, err)
	}
	if a.AccessKey != "" {
		_, _, err := ParseAccessKey(string(a.AccessKey))
		errs = appendIf(errs, err)
	}
	if a.Enabled && !a.HasBearer() && !a.HasAccessKey() {
		errs = append(errs, errors.New("authentication is on: set LOCH_RUNTIME_TOKEN_SHA256 (or another bearer "+
			"source) and/or LOCH_RUNTIME_ACCESS_KEY(_FILE), or LOCH_RUNTIME_AUTH=off"))
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

// ParseTokenSHA256 decodes the hex SHA-256 of a bearer token.
func ParseTokenSHA256(v string) ([32]byte, error) {
	var sum [32]byte
	b, err := hex.DecodeString(strings.TrimSpace(v))
	if err != nil || len(b) != len(sum) {
		return sum, errors.New("the bearer token SHA-256 must be 64 hexadecimal characters")
	}
	copy(sum[:], b)
	return sum, nil
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
