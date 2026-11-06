// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package invokeapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/loch-sh/lambda-runtime/internal/engine"
)

// browserRequests are header sets that browsers send, and nothing else.
var browserRequests = map[string]http.Header{
	"cross-site Origin":  {"Origin": {"http://evil.example"}, "Content-Type": {"text/plain"}},
	"null Origin":        {"Origin": {"null"}},
	"empty Origin":       {"Origin": {""}},
	"same-origin fetch":  {"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}},
	"Sec-Fetch-Site":     {"Sec-Fetch-Site": {"cross-site"}},
	"Sec-Fetch-Dest":     {"Sec-Fetch-Dest": {"document"}},
	"navigation":         {"Sec-Fetch-User": {"?1"}},
	"lower-case on wire": {"sec-fetch-dest": {"empty"}},
}

// TestBrowserRequestsAreRefused: a web page must not make a browser invoke
// the function (CSRF with a "simple" text/plain POST, or DNS rebinding,
// which makes the request same-origin), whatever the authentication.
func TestBrowserRequestsAreRefused(t *testing.T) {
	invoked := 0
	count := func(job *engine.Job) { invoked++; echo(job) }
	obs := &countObserver{}
	setups := map[string]Options{
		"authentication off": {},
		// The loopback is exempt from authentication, not from this.
		"loopback exempt from authentication": {Auth: denyAll{}, LoopbackWithoutAuth: true},
		// Refused before the authentication (401 otherwise).
		"authentication required": {Auth: denyAll{}},
	}
	for setup, opts := range setups {
		opts.Observer, opts.Metrics = obs, fixedMetrics("m")
		f := newFixture(t, opts, count)
		for name, h := range browserRequests {
			t.Run(setup+"/"+name, func(t *testing.T) {
				resp, body := f.do("POST", invokePath, strings.Repeat("x", 2000), h)
				wantAWSError(t, resp, body, 403, "AccessDeniedException")
				if !resp.Close {
					t.Error("connection kept")
				}
				if strings.Contains(string(body), "LOCH_RUNTIME") {
					t.Errorf("the answer names the setting: %s", body)
				}
				resp, body = f.do("GET", "/_loch/metrics", "", h)
				wantAWSError(t, resp, body, 403, "AccessDeniedException")
			})
		}
	}
	if invoked != 0 {
		t.Errorf("the function was invoked %d times", invoked)
	}
	if obs.authFailed != 0 || obs.throttled != 0 {
		t.Errorf("browser refusals counted as auth failures (%d) or throttling (%d)", obs.authFailed, obs.throttled)
	}
}

// TestBrowserRefusalExceptions: what the refusal must leave alone.
func TestBrowserRefusalExceptions(t *testing.T) {
	browser := browserRequests["same-origin fetch"]
	f := newFixture(t, Options{Metrics: fixedMetrics("m")}, echo)
	t.Run("health stays open to probes", func(t *testing.T) {
		if resp, _ := f.do("GET", "/_loch/health", "", browser); resp.StatusCode != 200 {
			t.Errorf("health from a browser: %d", resp.StatusCode)
		}
	})
	t.Run("plain clients", func(t *testing.T) {
		if resp, _ := f.do("POST", invokePath, `{}`, nil); resp.StatusCode != 200 {
			t.Errorf("plain client: %d", resp.StatusCode)
		}
	})
	t.Run("Node's fetch sends Sec-Fetch-Mode alone", func(t *testing.T) {
		node := http.Header{"Sec-Fetch-Mode": {"cors"}, "Content-Type": {"text/plain;charset=UTF-8"}}
		if resp, _ := f.do("POST", invokePath, `{}`, node); resp.StatusCode != 200 {
			t.Errorf("Node fetch: %d", resp.StatusCode)
		}
	})
	t.Run("the method is checked first", func(t *testing.T) {
		if resp, _ := f.do("POST", "/_loch/metrics", "", browser); resp.StatusCode != 405 {
			t.Errorf("POST metrics from a browser: %d", resp.StatusCode)
		}
	})
	t.Run("LOCH_RUNTIME_ALLOW_BROWSER=true", func(t *testing.T) {
		allowed := newFixture(t, Options{AllowBrowser: true, Metrics: fixedMetrics("m")}, echo)
		if resp, _ := allowed.do("POST", invokePath, `{}`, browser); resp.StatusCode != 200 {
			t.Errorf("allowed browser invoke: %d", resp.StatusCode)
		}
		if resp, _ := allowed.do("GET", "/_loch/metrics", "", browser); resp.StatusCode != 200 {
			t.Errorf("allowed browser metrics: %d", resp.StatusCode)
		}
	})
}
