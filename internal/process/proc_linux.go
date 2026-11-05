// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package process

import (
	"os"
	"strconv"
)

// processTable describes every visible process, from /proc. Processes that
// vanish while it is read are skipped.
func processTable() (map[int]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	table := make(map[int]procInfo, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		if info, ok := parseStat(stat); ok {
			table[pid] = info
		}
	}
	return table, nil
}
