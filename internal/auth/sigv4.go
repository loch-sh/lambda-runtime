// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	sigV4Algorithm  = "AWS4-HMAC-SHA256"
	sigV4Service    = "lambda"
	sigV4Terminator = "aws4_request"
	amzDateLayout   = "20060102T150405Z"
	// maxSkew is the accepted distance between the request time and ours.
	maxSkew = 5 * time.Minute
	// unsignedPayload is refused: the body must be covered by the signature.
	unsignedPayload = "UNSIGNED-PAYLOAD"
)

// sigV4Auth is a parsed SigV4 Authorization header.
type sigV4Auth struct {
	accessKeyID   string
	date          string // YYYYMMDD of the credential scope
	region        string
	service       string
	terminator    string
	signedHeaders []string
	signature     []byte
}

// scope is the credential scope: date/region/service/aws4_request.
func (a *sigV4Auth) scope() string {
	return a.date + "/" + a.region + "/" + a.service + "/" + a.terminator
}

// parseSigV4 parses "AWS4-HMAC-SHA256 Credential=…, SignedHeaders=…,
// Signature=…".
func parseSigV4(header string) (*sigV4Auth, error) {
	fields := map[string]string{}
	for part := range strings.SplitSeq(strings.TrimPrefix(header, sigV4Algorithm+" "), ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, errMalformed("Authorization header is malformed.")
		}
		fields[key] = value
	}
	cred := strings.Split(fields["Credential"], "/")
	if len(cred) != 5 || fields["SignedHeaders"] == "" || fields["Signature"] == "" {
		return nil, errMalformed("Authorization header requires Credential, SignedHeaders and Signature.")
	}
	sig, err := hex.DecodeString(fields["Signature"])
	if err != nil || len(sig) != sha256.Size {
		return nil, errMalformed("Signature must be 64 hexadecimal characters.")
	}
	a := &sigV4Auth{
		accessKeyID: cred[0], date: cred[1], region: cred[2], service: cred[3], terminator: cred[4],
		signedHeaders: strings.Split(fields["SignedHeaders"], ";"),
		signature:     sig,
	}
	if a.service != sigV4Service || a.terminator != sigV4Terminator || a.region == "" {
		return nil, errMalformed("Credential should be scoped to a valid region and the lambda service.")
	}
	if !slices.IsSorted(a.signedHeaders) || !slices.Contains(a.signedHeaders, "host") {
		return nil, errMalformed("SignedHeaders must be sorted and include host.")
	}
	return a, nil
}

// verifySigV4 checks a SigV4 request. When the payload hash is in the
// X-Amz-Content-Sha256 header, the signature is verified now and the body
// is later checked against that hash; otherwise the signature, which
// covers the body, is verified once the body is read (Pending).
func verifySigV4(r *http.Request, header string, c *Credentials, now time.Time) (BodyCheck, error) {
	a, err := parseSigV4(header)
	if err != nil {
		return BodyCheck{}, err
	}
	if !sameSecret(a.accessKeyID, c.AccessKeyID) {
		return BodyCheck{}, errBadToken
	}
	if err := checkSignedHeaders(r, a.signedHeaders); err != nil {
		return BodyCheck{}, err
	}
	t, err := requestTime(r, a)
	if err != nil {
		return BodyCheck{}, err
	}
	amzDate := t.UTC().Format(amzDateLayout)
	if !strings.HasPrefix(amzDate, a.date) {
		return BodyCheck{}, errMalformed("The request date does not match the credential scope.")
	}
	if d := now.Sub(t); d > maxSkew || d < -maxSkew {
		return BodyCheck{}, errExpired()
	}
	signer := &requestSigner{auth: a, request: r, amzDate: amzDate, secret: c.SecretAccessKey}
	switch hash := r.Header.Get("X-Amz-Content-Sha256"); hash {
	case unsignedPayload:
		return BodyCheck{}, errMalformed("UNSIGNED-PAYLOAD is not accepted: sign the payload.")
	case "":
		return BodyCheck{Verify: func(body []byte) error { return signer.verify(sha256Hex(body)) }, Pending: true}, nil
	default:
		if err := signer.verify(hash); err != nil {
			return BodyCheck{}, err
		}
		return BodyCheck{Verify: func(body []byte) error {
			if !hmac.Equal([]byte(sha256Hex(body)), []byte(strings.ToLower(hash))) {
				return errBadSignature
			}
			return nil
		}}, nil
	}
}

// sameSecret compares two values in constant time, whatever their lengths.
func sameSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return hmac.Equal(ha[:], hb[:])
}

// checkSignedHeaders requires, as AWS does, that every x-amz-* header of
// the request and its content-type are signed: they drive the invocation
// (X-Amz-Log-Type, X-Amz-Client-Context, …) and must not be added or
// changed by someone replaying a signed request.
func checkSignedHeaders(r *http.Request, signed []string) error {
	for name := range r.Header {
		lower := strings.ToLower(name)
		mustSign := strings.HasPrefix(lower, "x-amz-") || lower == "content-type"
		if mustSign && lower != "x-amz-content-sha256" && !slices.Contains(signed, lower) {
			return errMalformed("Header " + name + " must be signed.")
		}
	}
	if r.Header.Get("X-Amz-Content-Sha256") != "" && !slices.Contains(signed, "x-amz-content-sha256") {
		return errMalformed("Header X-Amz-Content-Sha256 must be signed.")
	}
	return nil
}

// requestTime returns the signed time of the request: X-Amz-Date, or the
// Date header when X-Amz-Date is absent. The header used must be signed.
func requestTime(r *http.Request, a *sigV4Auth) (time.Time, error) {
	if v := r.Header.Get("X-Amz-Date"); v != "" {
		t, err := time.Parse(amzDateLayout, v)
		if err != nil || !slices.Contains(a.signedHeaders, "x-amz-date") {
			return time.Time{}, errMalformed("X-Amz-Date must be a signed ISO 8601 basic date.")
		}
		return t, nil
	}
	if v := r.Header.Get("Date"); v != "" {
		t, err := http.ParseTime(v)
		if err != nil || !slices.Contains(a.signedHeaders, "date") {
			return time.Time{}, errMalformed("Date must be a signed HTTP date.")
		}
		return t, nil
	}
	return time.Time{}, errMalformed("The request must carry a signed X-Amz-Date or Date header.")
}

// requestSigner recomputes the signature of a request.
type requestSigner struct {
	auth    *sigV4Auth
	request *http.Request
	amzDate string
	secret  string
}

func (s *requestSigner) verify(payloadHash string) error {
	want := signature(s.secret, s.auth.date, s.auth.region, s.auth.service,
		stringToSign(s.amzDate, s.auth.scope(), canonicalRequest(s.request, s.auth.signedHeaders, payloadHash)))
	if !hmac.Equal(want, s.auth.signature) {
		return errBadSignature
	}
	return nil
}

// canonicalRequest builds the SigV4 canonical request. As for every
// service but S3, the path is URI-encoded once more (the AWS SDKs sign the
// already-encoded path encoded again).
func canonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) string {
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return strings.Join([]string{
		r.Method,
		uriEncode(path, false),
		canonicalQuery(r.URL.RawQuery),
		canonicalHeaders(r, signedHeaders),
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")
}

// canonicalQuery encodes the query parameters, sorted by encoded key, then
// by encoded value.
func canonicalQuery(raw string) string {
	values, _ := url.ParseQuery(raw)
	type pair struct{ key, value string }
	var pairs []pair
	for key, vs := range values {
		for _, v := range vs {
			pairs = append(pairs, pair{uriEncode(key, true), uriEncode(v, true)})
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		if c := strings.Compare(a.key, b.key); c != 0 {
			return c
		}
		return strings.Compare(a.value, b.value)
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.key + "=" + p.value
	}
	return strings.Join(out, "&")
}

// canonicalHeaders lists the signed headers, lowercase, values trimmed and
// inner spaces collapsed, each line ending with a newline.
func canonicalHeaders(r *http.Request, names []string) string {
	var b strings.Builder
	for _, name := range names {
		var raw []string
		switch name {
		case "host":
			raw = []string{r.Host}
		case "content-length":
			// net/http moves it out of Header. SDKs sign it only when the
			// length is known; a chunked request would sign "-1" and fail.
			raw = []string{strconv.FormatInt(r.ContentLength, 10)}
		default:
			raw = r.Header.Values(name)
		}
		values := make([]string, len(raw)) // never modify the request
		for i, v := range raw {
			values[i] = strings.Join(strings.Fields(v), " ")
		}
		b.WriteString(name + ":" + strings.Join(values, ",") + "\n")
	}
	return b.String()
}

func stringToSign(amzDate, scope, canonical string) string {
	return sigV4Algorithm + "\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
}

// signature derives the signing key and signs toSign.
func signature(secret, date, region, service, toSign string) []byte {
	key := hmacSHA256([]byte("AWS4"+secret), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, sigV4Terminator)
	return hmacSHA256(key, toSign)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// uriEncode percent-encodes every byte but the RFC 3986 unreserved ones
// (and '/' unless encodeSlash), with uppercase hex, as SigV4 requires.
func uriEncode(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}
