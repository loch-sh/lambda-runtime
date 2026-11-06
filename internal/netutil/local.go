// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package netutil

import "net"

// LocalAddr turns a listen address into one to connect to from inside the
// container: the wildcard hosts become the IPv4 loopback.
func LocalAddr(listen string) string {
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
