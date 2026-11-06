// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Sign signs r with SigV4 for the lambda service, as the AWS SDKs do:
// host, x-amz-date, content-type and every x-amz-* header already set are
// signed, the payload hash is in the signature only. Set the headers
// before signing. (Test client: the runtime itself never signs.)
func Sign(r *http.Request, body []byte, keyID, secret, region string, at time.Time) {
	signWith(r, body, keyID, secret, region, at, signOptions{})
}

// signOptions are the variants of SigV4 signing that other clients use.
type signOptions struct {
	// hashHeader sends and signs X-Amz-Content-Sha256 (S3-style clients).
	hashHeader bool
	// dateHeader signs the Date header instead of X-Amz-Date.
	dateHeader bool
}

func signWith(r *http.Request, body []byte, keyID, secret, region string, at time.Time, o signOptions) {
	amzDate := at.UTC().Format(amzDateLayout)
	date := amzDate[:8]
	names := []string{"host"}
	for name := range r.Header {
		if lower := strings.ToLower(name); lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
			names = append(names, lower)
		}
	}
	if o.dateHeader {
		r.Header.Set("Date", at.UTC().Format(http.TimeFormat))
		names = append(names, "date")
	} else {
		r.Header.Set("X-Amz-Date", amzDate)
		names = append(names, "x-amz-date")
	}
	if o.hashHeader {
		r.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
		names = append(names, "x-amz-content-sha256")
	}
	slices.Sort(names)
	names = slices.Compact(names)
	scope := date + "/" + region + "/" + sigV4Service + "/" + sigV4Terminator
	sig := signature(secret, date, region, sigV4Service,
		stringToSign(amzDate, scope, canonicalRequest(r, names, sha256Hex(body))))
	r.Header.Set("Authorization", sigV4Algorithm+" Credential="+keyID+"/"+scope+
		", SignedHeaders="+strings.Join(names, ";")+", Signature="+hex.EncodeToString(sig))
}
