// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package auth

import (
	"errors"
	"syscall"
)

const (
	// accessWrite is W_OK of access(2). access checks the real uid, equal
	// to the effective one here (no setuid).
	accessWrite = 0x2
	// readOnlyMount is ST_RDONLY (linux) and MNT_RDONLY (darwin) in the
	// flags of statfs(2): both are 1.
	readOnlyMount = 0x1
)

// inspectPath returns the owner of p, whether its file system is
// read-only, and whether the runtime may write it.
func inspectPath(p string) (pathState, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return pathState{}, err
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(p, &fs); err != nil {
		return pathState{}, err
	}
	return pathState{
		uid:      int(st.Uid),
		readOnly: uint64(fs.Flags)&readOnlyMount != 0,
		writeErr: access(p),
	}, nil
}

func access(p string) error {
	for {
		err := syscall.Access(p, accessWrite)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
