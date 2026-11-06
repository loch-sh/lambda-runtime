// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/config"
)

// trustFiles disables the check of who can change the credential files:
// the temporary files of the tests belong to the user running them.
func trustFiles(t *testing.T) { t.Cleanup(TrustFilesForTests()) }

// writeFile replaces dir/name with a read-only file holding v and returns
// its path.
func writeFile(t *testing.T, dir, name, v string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(v), 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPathStateCheck(t *testing.T) {
	const runtime, root = 10001, 0 // the runtime runs as runtime
	for _, c := range []struct {
		name  string
		state pathState
		want  string // "" when accepted
	}{
		{"read-only mount, runtime's file", pathState{uid: runtime, readOnly: true}, ""},
		{"runtime's file, mode 0400", pathState{uid: runtime, writeErr: syscall.EACCES}, "belongs to the runtime's user"},
		{"another owner, not writable", pathState{uid: root, writeErr: syscall.EACCES}, ""},
		{"another owner, read-only (EROFS)", pathState{uid: root, writeErr: syscall.EROFS}, ""},
		{"another owner, writable (group or world)", pathState{uid: root}, "writable by the runtime"},
		{"unexpected access error", pathState{uid: root, writeErr: syscall.EIO}, "cannot tell"},
	} {
		err := c.state.check("/p", runtime)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// TestCheckPathElements: the file and every directory up to the root are
// checked, along the path and along the path its links lead to.
func TestCheckPathElements(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := writeFile(t, filepath.Join(dir, "real"), "token", testToken)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var seen []string
	readOnly := func(p string) (pathState, error) {
		seen = append(seen, p)
		return pathState{readOnly: true}, nil
	}
	if err := checkPathElements(link, 10001, readOnly); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{link, dir, target, filepath.Dir(target), "/"} {
		if !slices.Contains(seen, want) {
			t.Errorf("%s not checked (checked: %v)", want, seen)
		}
	}
	// An ancestor the runtime owns on a writable file system is refused,
	// even when the file itself is on a read-only mount: renaming the
	// ancestor would replace the file.
	ancestor := filepath.Dir(dir)
	err = checkPathElements(target, 10001, func(p string) (pathState, error) {
		if p == ancestor {
			return pathState{uid: 10001}, nil
		}
		return pathState{readOnly: true}, nil
	})
	if err == nil || !strings.Contains(err.Error(), ancestor) {
		t.Errorf("writable ancestor: %v", err)
	}
	failing := errors.New("statfs failed")
	if err := checkPathElements(target, 10001, func(string) (pathState, error) { return pathState{}, failing }); !errors.Is(err, failing) {
		t.Errorf("inspect error: %v", err)
	}
}

// TestRuntimeOwnFileIsRefused reproduces the bypass of a check on the mode
// bits alone: a 0400 file in a 0500 directory, both owned by the runtime's
// user on a writable file system, can be made writable again (chmod), then
// replaced before a SIGHUP.
func TestRuntimeOwnFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	file := writeFile(t, dir, "token", testToken)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := readCredentialFile(file)
	if err == nil || !strings.Contains(err.Error(), "belongs to the runtime's user") {
		t.Fatalf("own file accepted: %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Error("the error shows the credential")
	}
}

func TestCredentialFileTypeAndMode(t *testing.T) {
	trustFiles(t)
	dir := t.TempDir()
	if v, err := readCredentialFile(writeFile(t, dir, "ok", testToken+"\n")); err != nil || v != testToken {
		t.Errorf("regular file: %q %v", v, err)
	}
	worldWritable := writeFile(t, dir, "world", testToken)
	if err := os.Chmod(worldWritable, 0o602); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredentialFile(worldWritable); err == nil || !strings.Contains(err.Error(), "world-writable") {
		t.Errorf("world-writable: %v", err)
	}
	if _, err := readCredentialFile(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: %v", err)
	}
}

// TestCredentialFileFIFO: a FIFO swapped in for the file is refused at once
// instead of blocking the startup or the reload.
func TestCredentialFileFIFO(t *testing.T) {
	trustFiles(t)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o400); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readCredentialFile(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("fifo: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}

// TestReloadRefusesAFileTheRuntimeCanChange: the credentials in force are
// kept.
func TestReloadRefusesAFileTheRuntimeCanChange(t *testing.T) {
	file := writeFile(t, t.TempDir(), "token", testToken)
	s := trustedStore(t, config.Auth{Enabled: true, TokenFile: file})
	writeFile(t, filepath.Dir(file), "token", strings.Repeat("e", 40))
	if err := s.Reload(); err == nil || !strings.Contains(err.Error(), "belongs to the runtime's user") {
		t.Errorf("reload: %v", err)
	}
	if s.Current().TokenSHA256 != sha256.Sum256([]byte(testToken)) {
		t.Error("credentials in force lost")
	}
}

// trustedStore loads a store from files the check would refuse, then
// restores the check.
func trustedStore(t *testing.T, cfg config.Auth) *Store {
	t.Helper()
	restore := TrustFilesForTests()
	defer restore()
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTrustFilesOnlyInTests: production code never disables the check.
func TestTrustFilesOnlyInTests(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			filepath.Base(path) == "credfile.go" {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "TrustFilesForTests") {
			t.Errorf("%s calls TrustFilesForTests", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
