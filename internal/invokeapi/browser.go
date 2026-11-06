// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package invokeapi

import "net/http"

// browserHeaders are sent by web browsers and not by the other clients:
// modern browsers send Origin on every POST (same-origin included, "null"
// from a file:// page or a sandboxed frame), and Sec-Fetch-Site,
// Sec-Fetch-Dest (and Sec-Fetch-User on navigations) to trustworthy
// origins (HTTPS, localhost, 127.0.0.1). The AWS SDKs, the AWS CLI, curl,
// Deno and Python send none of them. Sec-Fetch-Mode alone is not one:
// Node's fetch (undici) sends "Sec-Fetch-Mode: cors" by itself.
var browserHeaders = []string{"Origin", "Sec-Fetch-Site", "Sec-Fetch-Dest", "Sec-Fetch-User"}

// fromBrowser reports whether r was sent by a web browser. A page could
// otherwise make the browser invoke the function: a "simple" cross-site
// POST (text/plain) needs no CORS preflight, and DNS rebinding makes the
// request same-origin, which is why the values do not matter.
func fromBrowser(r *http.Request) bool {
	for _, name := range browserHeaders {
		// The server canonicalizes the header names it reads.
		if _, ok := r.Header[name]; ok {
			return true
		}
	}
	return false
}

// refuseBrowser answers 403 to a browser request unless browsers are
// allowed, and reports whether it did. The answer does not name the
// setting that would allow it: the caller is a page, not an operator.
func (h *Handler) refuseBrowser(w http.ResponseWriter, r *http.Request, id string) bool {
	if h.opts.AllowBrowser || !fromBrowser(r) {
		return false
	}
	w.Header().Set("Connection", "close")
	if h.browserRefusalLog.allow(r.RemoteAddr) {
		h.opts.Log.Warn("invoke: browser request refused (LOCH_RUNTIME_ALLOW_BROWSER=true allows them)", "remote", r.RemoteAddr)
	}
	errBrowser().write(w, id)
	return true
}
