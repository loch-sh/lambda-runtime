// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/sha256"
	"crypto/subtle"
)

// checkBearer compares the SHA-256 of the presented token with the one
// configured, in constant time: only the hash is ever held.
func checkBearer(c *Credentials, presented string) error {
	if !c.HasToken {
		return errBadToken
	}
	got := sha256.Sum256([]byte(presented))
	if subtle.ConstantTimeCompare(c.TokenSHA256[:], got[:]) != 1 {
		return errBadToken
	}
	return nil
}
