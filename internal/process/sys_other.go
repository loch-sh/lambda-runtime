// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux

package process

// Non-Linux Unix systems are supported for development and tests only:
// these Linux features are no-ops there.

func becomeSubreaper() error { return nil }

func disableDumping() error { return nil }

func setOOMScoreAdj(pid, value int) error { return nil }
