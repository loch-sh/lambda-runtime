// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package auth

import "errors"

// inspectPath is implemented on Linux (the target) and macOS (development)
// only: elsewhere, credential files are refused.
func inspectPath(string) (pathState, error) {
	return pathState{}, errors.New("credential files are supported on Linux and macOS only")
}
