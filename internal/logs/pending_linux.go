// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logs

import (
	"os"
	"syscall"
	"unsafe"
)

// fionread is FIONREAD on Linux.
const fionread = 0x541B

// pending returns how many bytes are waiting in a pipe.
func pending(f *os.File) (int, bool) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, false
	}
	var n int32
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, fionread, uintptr(unsafe.Pointer(&n)))
	}); err != nil || errno != 0 {
		return 0, false
	}
	return int(n), true
}
