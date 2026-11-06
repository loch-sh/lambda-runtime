// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/loch-sh/lambda-runtime/internal/auth"
	"github.com/loch-sh/lambda-runtime/internal/config"
	"github.com/loch-sh/lambda-runtime/internal/invokeapi"
)

// secure runs before anything else: given the result of making the
// process non-dumpable (done first by main), it loads the credentials and
// removes them from our environment. It returns the credential store and
// the authenticator, both nil when authentication is off.
func secure(cfg *config.Config, hardenErr error, log *slog.Logger) (*auth.Store, invokeapi.Authenticator, error) {
	defer clearSecretEnv()
	if err := hardenErr; err != nil {
		// os.Unsetenv does not clear /proc/1/environ: only a non-dumpable
		// process keeps the child from reading credentials there.
		if credentialsInEnv(cfg.Runtime.Auth) {
			return nil, nil, errors.New("cannot make the process non-dumpable while credentials come from the environment: " + err.Error())
		}
		log.Warn("process not made non-dumpable", "error", err)
	}
	if !cfg.Runtime.Auth.Enabled {
		log.Warn("authentication is off: anyone who can reach the Invoke API can invoke the function")
		return nil, nil, nil
	}
	store, err := auth.NewStore(cfg.Runtime.Auth)
	if err != nil {
		return nil, nil, err
	}
	return store, auth.New(store), nil
}

// credentialsInEnv reports whether a secret came from the environment (a
// token hash is not a secret).
func credentialsInEnv(a config.Auth) bool { return a.Enabled && (a.Token != "" || a.AccessKey != "") }

func clearSecretEnv() {
	for _, key := range config.SecretEnv {
		_ = os.Unsetenv(key)
	}
}

// reloadOnSIGHUP re-reads the credential files on SIGHUP (rotation) until
// the returned function is called.
func reloadOnSIGHUP(store *auth.Store, log *slog.Logger) (stop func()) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	done := make(chan struct{})
	go reloadLoop(hup, done, store, log)
	return func() {
		signal.Stop(hup)
		close(done)
	}
}

// reloadLoop reloads store on each signal; a failed reload keeps the
// credentials in force.
func reloadLoop(signals <-chan os.Signal, done <-chan struct{}, store *auth.Store, log *slog.Logger) {
	for {
		select {
		case <-done:
			return
		case <-signals:
			if err := store.Reload(); err != nil {
				log.Error("credentials not reloaded, the previous ones stay in force", "error", err)
				continue
			}
			log.Info("credentials reloaded")
		}
	}
}
