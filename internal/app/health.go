// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/config"
)

// healthTimeout bounds the health check.
const healthTimeout = 3 * time.Second

// Health queries /_loch/health on the loopback health listener: exit code
// 0 when the runtime is ready, 1 otherwise (a compose healthcheck that
// needs no curl, and never competes with Invoke API clients).
func Health(cfg *config.Config, stdout io.Writer) int {
	url := "http://" + localAddr(cfg.Runtime.HealthAddr) + "/_loch/health"
	client := &http.Client{Timeout: healthTimeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(stdout, "unreachable:", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Fprintln(stdout, string(body))
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// localAddr turns a listen address into one to connect to: the wildcard
// hosts become the loopback.
func localAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
