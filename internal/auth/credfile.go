// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxCredentialFile bounds the size of a credential file.
const maxCredentialFile = 4096

// readCredentialFile reads a credential file: a regular file of at most
// maxCredentialFile bytes, rotated by renaming a complete new file over it,
// then SIGHUP.
//
// Why the checks: the function's code runs under the runtime's user and can
// send it SIGHUP. A credential file it could write or replace would let it
// install a credential of its own. So the file must be one the runtime's
// user cannot change (see checkUnchangeable), and it is opened without
// blocking, so that a FIFO swapped in is refused instead of waited on.
func readCredentialFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// The type and the mode are checked on the descriptor, which is what is
	// read.
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	switch {
	case !info.Mode().IsRegular():
		return "", fmt.Errorf("%s is not a regular file", path)
	case info.Mode().Perm()&0o002 != 0:
		return "", fmt.Errorf("%s is world-writable", path)
	}
	// Who can change it is checked on the path, not on the descriptor.
	if err := checkUnchangeable(path); err != nil {
		return "", err
	}
	// Then the path must still name the opened file: this closes the
	// window between the open and the checks of the path.
	if now, err := os.Stat(path); err != nil || !os.SameFile(info, now) {
		return "", fmt.Errorf("%s changed while it was read", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCredentialFile+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxCredentialFile {
		return "", fmt.Errorf("%s is larger than %d bytes", path, maxCredentialFile)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return v, nil
}

// checkUnchangeable refuses a credential file that the runtime's user could
// change. It is a variable so that tests, whose temporary files belong to
// the user running them, can trust their files (TrustFilesForTests).
var checkUnchangeable = runtimeCannotChange

// runtimeCannotChange checks the file and every directory up to the root,
// both along the path and along the path its symbolic links lead to: a
// writable directory anywhere above lets the file be replaced (by renaming
// a directory). The directories holding the intermediate links of a chain
// are not checked.
func runtimeCannotChange(path string) error {
	return checkPathElements(path, os.Geteuid(), inspectPath)
}

// checkPathElements checks path and its directories with inspect, for the
// runtime's user euid.
func checkPathElements(path string, euid int, inspect func(string) (pathState, error)) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	target, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	checked := map[string]bool{}
	for _, start := range []string{abs, target} {
		// Up to "/" (filepath.Dir("/") is "/"), or to a directory seen
		// already.
		for p := start; !checked[p]; p = filepath.Dir(p) {
			checked[p] = true
			state, err := inspect(p)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if err := state.check(p, euid); err != nil {
				return err
			}
		}
	}
	return nil
}

// pathState is what decides whether the runtime's user can change a path.
type pathState struct {
	// uid owns the path.
	uid int
	// readOnly reports a read-only file system (a :ro mount).
	readOnly bool
	// writeErr is the result of access(W_OK) on the path.
	writeErr error
}

// check refuses a path that the runtime's user euid could change. On a
// read-only file system, nothing can. Otherwise the owner can always make
// its path writable (chmod), whatever its mode: a path of the runtime's
// user is refused. For another owner, the kernel tells (access(2)); an
// unexpected error refuses too.
func (s pathState) check(p string, euid int) error {
	switch {
	case s.readOnly:
		return nil
	case s.uid == euid:
		return fmt.Errorf("%s belongs to the runtime's user on a writable file system: mount the credential files read-only", p)
	case s.writeErr == nil:
		return fmt.Errorf("%s is writable by the runtime: mount the credential files read-only", p)
	case errors.Is(s.writeErr, syscall.EACCES), errors.Is(s.writeErr, syscall.EPERM), errors.Is(s.writeErr, syscall.EROFS):
		return nil
	default:
		return fmt.Errorf("%s: cannot tell whether the runtime can write it: %w", p, s.writeErr)
	}
}

// TrustFilesForTests disables the check of who can change the credential
// files, for the tests of other packages, whose temporary files belong to
// the user running them on a writable file system. It returns the function
// that restores the check. Production code never calls it (guarded by
// TestTrustFilesOnlyInTests). It is exported because the tests of
// internal/app need it too; it is not safe for parallel tests.
func TrustFilesForTests() (restore func()) {
	checkUnchangeable = func(string) error { return nil }
	return func() { checkUnchangeable = runtimeCannotChange }
}
