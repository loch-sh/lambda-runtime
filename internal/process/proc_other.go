// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux

package process

// processTable is empty without /proc: KillDescendants is a no-op.
func processTable() (map[int]procInfo, error) { return map[int]procInfo{}, nil }
