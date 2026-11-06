// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// validateAddr checks a host:port listen address. The host must be an IP
// (or empty for all interfaces), and a loopback IPv4 address when
// loopbackOnly is set: the Runtime API address is handed to the runtime
// clients, and the Node.js one splits it on ':', which breaks on IPv6.
func validateAddr(addr string, loopbackOnly bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("%q has an invalid port", addr)
	}
	if host == "" && !loopbackOnly {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	switch {
	case loopbackOnly && (err != nil || !ip.Is4() || !ip.IsLoopback()):
		return fmt.Errorf("%q: must be a loopback IPv4 address", addr)
	case err != nil:
		return fmt.Errorf("%q: host must be an IP address", addr)
	}
	return nil
}

// parseDuration accepts a Go duration ("10s", "1m30s") or a plain number
// of seconds ("10", "2.5"), between 0 and limit.
func parseDuration(v string, limit time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		secs, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil || math.IsNaN(secs) || math.IsInf(secs, 0) {
			return 0, fmt.Errorf("%q is neither a duration (10s) nor a number of seconds", v)
		}
		if secs < 0 || secs > limit.Seconds() {
			return 0, fmt.Errorf("%q is out of range [0, %s]", v, limit)
		}
		d = time.Duration(secs * float64(time.Second))
	}
	if d < 0 || d > limit {
		return 0, fmt.Errorf("%q is out of range [0, %s]", v, limit)
	}
	return d, nil
}

// parseSize accepts a number of bytes, or one with a KiB, MiB or GiB
// suffix. Range checks are left to the caller.
func parseSize(v string) (int64, error) {
	num, mult := v, int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}} {
		if s, ok := strings.CutSuffix(v, u.suffix); ok {
			num, mult = strings.TrimSpace(s), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("%q is not a size (bytes, or a KiB, MiB or GiB suffix)", v)
	}
	return n * mult, nil
}

func validFunctionName(s string) bool {
	return validChars(s, "-_")
}

// validToken accepts the characters of versions ($LATEST, 42) and regions
// (eu-west-3).
func validToken(s string) bool {
	return validChars(s, "-_$")
}

// validChars reports whether s has 1 to 64 ASCII letters, digits or
// characters of extra.
func validChars(s, extra string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && !strings.ContainsRune(extra, r) {
			return false
		}
	}
	return true
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
