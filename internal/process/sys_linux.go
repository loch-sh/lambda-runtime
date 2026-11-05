// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package process

import (
	"os"
	"strconv"
	"syscall"
)

// prctl options from linux/prctl.h.
const (
	prSetDumpable       = 4
	prSetChildSubreaper = 36
)

func prctl(option, arg uintptr) error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, option, arg, 0); errno != 0 {
		return errno
	}
	return nil
}

// becomeSubreaper makes orphaned descendants reparent to this process
// instead of PID 1, so that they are reaped here.
func becomeSubreaper() error { return prctl(prSetChildSubreaper, 1) }

func disableDumping() error { return prctl(prSetDumpable, 0) }

// setOOMScoreAdj raises the OOM score of a process; raising it needs no
// privilege.
func setOOMScoreAdj(pid, value int) error {
	return os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", []byte(strconv.Itoa(value)), 0)
}
