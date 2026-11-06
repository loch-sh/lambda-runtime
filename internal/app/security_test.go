// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/auth"
	"github.com/loch-sh/lambda-runtime/internal/config"
)

const securityToken = "0123456789abcdef0123456789abcdef"

func TestHardenFailureIsFatalWithEnvCredentials(t *testing.T) {
	refused := errors.New("prctl refused")
	cfg := &config.Config{}
	cfg.Runtime.Auth = config.Auth{Enabled: true, Token: securityToken}
	if _, _, err := secure(cfg, refused, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "non-dumpable") {
		t.Errorf("err = %v", err)
	}
	// With file credentials (or none), it is only a warning.
	t.Cleanup(auth.TrustFilesForTests())
	file := writeToken(t, t.TempDir(), securityToken)
	cfg.Runtime.Auth = config.Auth{Enabled: true, TokenFile: file}
	if store, a, err := secure(cfg, refused, slog.New(slog.DiscardHandler)); err != nil || store == nil || a == nil {
		t.Errorf("file credentials: %v", err)
	}
}

func TestSecretEnvCleared(t *testing.T) {
	t.Setenv("LOCH_RUNTIME_TOKEN", securityToken)
	cfg := &config.Config{}
	cfg.Runtime.Auth = config.Auth{Enabled: true, Token: securityToken}
	if _, _, err := secure(cfg, nil, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv("LOCH_RUNTIME_TOKEN"); ok {
		t.Error("LOCH_RUNTIME_TOKEN still in the environment")
	}
}

func TestReloadLoop(t *testing.T) {
	t.Cleanup(auth.TrustFilesForTests())
	dir := t.TempDir()
	file := writeToken(t, dir, securityToken)
	store, err := auth.NewStore(config.Auth{Enabled: true, TokenFile: file})
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	signals := make(chan os.Signal)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() { reloadLoop(signals, done, store, log); close(finished) }()

	rotated := strings.Repeat("r", 40)
	writeToken(t, dir, rotated)
	signals <- syscall.SIGHUP
	waitUntil(t, func() bool { return store.Current().TokenSHA256 == sha256.Sum256([]byte(rotated)) })

	writeToken(t, dir, "short") // broken: the rotated token stays
	signals <- syscall.SIGHUP
	waitUntil(t, func() bool { return strings.Contains(logs.String(), "not reloaded") })
	if store.Current().TokenSHA256 != sha256.Sum256([]byte(rotated)) {
		t.Error("a broken reload replaced the credentials")
	}
	close(done)
	<-finished
}

func TestReloadOnSIGHUPStops(t *testing.T) {
	store, err := auth.NewStore(config.Auth{Enabled: true, Token: securityToken})
	if err != nil {
		t.Fatal(err)
	}
	stop := reloadOnSIGHUP(store, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	stop()
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// writeToken replaces dir/token with a file holding v (renamed over the old
// one, as a rotation does) and returns its path.
func writeToken(t *testing.T, dir, v string) string {
	t.Helper()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path+".new", []byte(v), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	return path
}
