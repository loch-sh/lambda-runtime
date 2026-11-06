// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package logs

import "os"

// pending is unknown outside Linux: Drain does not wait there.
func pending(*os.File) (int, bool) { return 0, false }
