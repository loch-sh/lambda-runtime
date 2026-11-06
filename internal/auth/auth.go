// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package auth authenticates the callers of the Invoke API with a bearer
// token or AWS Signature Version 4 (service "lambda", any region), so that
// AWS SDKs pointed at the runtime with endpoint-url work unchanged.
//
// Every check that does not need the body runs before it is read. A SigV4
// signature covers the payload hash. When the hash is in the
// X-Amz-Content-Sha256 header, the signature is verified at once and the
// body is only checked against the hash. The AWS SDKs for Lambda do not send
// that header: the signature itself is then verified once the body is read,
// and the caller is not authenticated until then (BodyCheck.Pending).
package auth

import (
	"net/http"
	"strings"
	"time"
)

// Error is an authentication failure, in the AWS error vocabulary.
type Error struct {
	Status  int
	Name    string
	Message string
}

func (e *Error) Error() string { return e.Name + ": " + e.Message }

var (
	errMissing = &Error{http.StatusUnauthorized, "MissingAuthenticationTokenException",
		"Missing Authentication Token"}
	errBadToken = &Error{http.StatusForbidden, "UnrecognizedClientException",
		"The security token included in the request is invalid."}
	errBadSignature = &Error{http.StatusForbidden, "InvalidSignatureException",
		"The request signature we calculated does not match the signature you provided."}
)

func errMalformed(detail string) *Error {
	return &Error{http.StatusForbidden, "IncompleteSignatureException", detail}
}

func errExpired() *Error {
	return &Error{http.StatusForbidden, "InvalidSignatureException", "Signature expired: the request time is more than 5 minutes away from the server time."}
}

// BodyCheck is what remains to check once the body is read. The zero value
// checks nothing.
type BodyCheck struct {
	// Verify checks the body; nil when nothing is left to check.
	Verify func(body []byte) error
	// Pending reports that the caller is not authenticated until Verify
	// passes (it checks the signature). Otherwise the caller is
	// authenticated already, and Verify only checks the body's integrity.
	Pending bool
}

// Check runs Verify, if any, on body.
func (c BodyCheck) Check(body []byte) error {
	if c.Verify == nil {
		return nil
	}
	return c.Verify(body)
}

// Authenticator checks requests against the configured credentials.
type Authenticator struct {
	creds *Store
	now   func() time.Time
}

// New returns an authenticator reading its credentials from creds.
func New(creds *Store) *Authenticator {
	return &Authenticator{creds: creds, now: time.Now}
}

// Authenticate checks the credential of r without reading its body.
func (a *Authenticator) Authenticate(r *http.Request) (BodyCheck, error) {
	c := a.creds.Current()
	header := r.Header.Get("Authorization")
	switch {
	case header == "":
		return BodyCheck{}, errMissing
	case strings.HasPrefix(header, "Bearer "):
		return BodyCheck{}, checkBearer(c, strings.TrimPrefix(header, "Bearer "))
	case strings.HasPrefix(header, sigV4Algorithm+" "):
		if c.AccessKeyID == "" {
			return BodyCheck{}, errBadToken
		}
		return verifySigV4(r, header, c, a.now())
	default:
		return BodyCheck{}, errMalformed("Unsupported Authorization header: use a bearer token or AWS4-HMAC-SHA256.")
	}
}
