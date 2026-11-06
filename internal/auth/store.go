// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/loch-sh/lambda-runtime/internal/config"
)

// Credentials are the accepted credentials. Only the SHA-256 of the bearer
// token is kept; an empty AccessKeyID disables SigV4.
type Credentials struct {
	TokenSHA256     [32]byte
	HasToken        bool
	AccessKeyID     string
	SecretAccessKey string
}

// Store holds the credentials, reloadable from their files (SIGHUP).
type Store struct {
	cfg     config.Auth
	current atomic.Pointer[Credentials]
}

// NewStore loads the credentials described by cfg: the values given by the
// environment, or the files.
func NewStore(cfg config.Auth) (*Store, error) {
	s := &Store{cfg: cfg}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Current returns the credentials in force.
func (s *Store) Current() *Credentials { return s.current.Load() }

// Reload re-reads the credential files; the credentials in force are kept
// when they fail to load.
func (s *Store) Reload() error {
	var c Credentials
	var err error
	if c.TokenSHA256, c.HasToken, err = s.bearer(); err != nil {
		return fmt.Errorf("bearer token: %w", err)
	}
	key, err := value(string(s.cfg.AccessKey), s.cfg.AccessKeyFile)
	if err != nil {
		return fmt.Errorf("access key: %w", err)
	}
	if key != "" {
		if c.AccessKeyID, c.SecretAccessKey, err = config.ParseAccessKey(key); err != nil {
			return err
		}
	}
	if !c.HasToken && c.AccessKeyID == "" {
		return errors.New("no credential: the token and the access key are both empty")
	}
	s.current.Store(&c)
	return nil
}

// bearer returns the SHA-256 of the bearer token, from its single source:
// the token itself (hashed here) or its hash.
func (s *Store) bearer() ([32]byte, bool, error) {
	var none [32]byte
	token, err := value(string(s.cfg.Token), s.cfg.TokenFile)
	if err != nil {
		return none, false, err
	}
	if token != "" {
		if err := config.ValidateToken(token); err != nil {
			return none, false, err
		}
		return sha256.Sum256([]byte(token)), true, nil
	}
	hash, err := value(s.cfg.TokenSHA256, s.cfg.TokenSHA256File)
	if err != nil || hash == "" {
		return none, false, err
	}
	sum, err := config.ParseTokenSHA256(hash)
	return sum, err == nil, err
}

// value returns the value given directly, else the content of file.
func value(direct, file string) (string, error) {
	if direct != "" || file == "" {
		return direct, nil
	}
	return readCredentialFile(file)
}
