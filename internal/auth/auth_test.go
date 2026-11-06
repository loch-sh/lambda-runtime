// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/config"
)

const (
	testToken  = "0123456789abcdef0123456789abcdef"
	testKeyID  = "AKIDEXAMPLE"
	testSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

var testNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func newAuth(t *testing.T, cfg config.Auth) *Authenticator {
	t.Helper()
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := New(store)
	a.now = func() time.Time { return testNow }
	return a
}

func both(t *testing.T) *Authenticator {
	return newAuth(t, config.Auth{Enabled: true, Token: testToken, AccessKey: config.Secret(testKeyID + ":" + testSecret)})
}

func wantAuthError(t *testing.T, err error, status int, name string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != status || e.Name != name || e.Message == "" || e.Error() == "" {
		t.Fatalf("err = %v, want %d %s", err, status, name)
	}
}

// TestAWSReferenceVector checks the signing against the example of the AWS
// documentation (IAM ListUsers, 2015-08-30).
func TestAWSReferenceVector(t *testing.T) {
	r := httptest.NewRequest("GET", "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	r.Header.Set("X-Amz-Date", "20150830T123600Z")
	canonical := canonicalRequest(r, []string{"content-type", "host", "x-amz-date"}, sha256Hex(nil))
	wantCanonical := "GET\n/\nAction=ListUsers&Version=2010-05-08\n" +
		"content-type:application/x-www-form-urlencoded; charset=utf-8\nhost:iam.amazonaws.com\nx-amz-date:20150830T123600Z\n\n" +
		"content-type;host;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if canonical != wantCanonical {
		t.Fatalf("canonical request =\n%s\nwant\n%s", canonical, wantCanonical)
	}
	sig := signature(testSecret, "20150830", "us-east-1", "iam",
		stringToSign("20150830T123600Z", "20150830/us-east-1/iam/aws4_request", canonical))
	if got := hex.EncodeToString(sig); got != "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7" {
		t.Errorf("signature = %s", got)
	}
}

// sign signs r like an AWS SDK does for Lambda; with withHashHeader, the
// payload hash is also sent and signed (as S3 clients do).
func sign(r *http.Request, body []byte, keyID, secret string, at time.Time, withHashHeader bool) {
	signWith(r, body, keyID, secret, "eu-west-3", at, signOptions{hashHeader: withHashHeader})
}

const invokeURL = "http://fn:8080/2015-03-31/functions/my%3Afunction%3A%24LATEST/invocations?Qualifier=%24LATEST"

func signedRequest(body string, keyID, secret string, at time.Time, hashHeader bool) *http.Request {
	r := httptest.NewRequest("POST", invokeURL, strings.NewReader(body))
	sign(r, []byte(body), keyID, secret, at, hashHeader)
	return r
}

func TestSigV4Valid(t *testing.T) {
	a := both(t)
	for _, hashHeader := range []bool{false, true} {
		r := signedRequest(`{"a":1}`, testKeyID, testSecret, testNow.Add(-4*time.Minute), hashHeader)
		check, err := a.Authenticate(r)
		if err != nil {
			t.Fatalf("hash header %v: %v", hashHeader, err)
		}
		if err := check([]byte(`{"a":1}`)); err != nil {
			t.Errorf("hash header %v: body check: %v", hashHeader, err)
		}
		// A different body fails the check.
		wantAuthError(t, check([]byte(`{"a":2}`)), 403, "InvalidSignatureException")
	}
}

func TestSigV4Failures(t *testing.T) {
	a := both(t)
	r := signedRequest(`{}`, testKeyID, testSecret, testNow.Add(-6*time.Minute), false)
	_, err := a.Authenticate(r)
	wantAuthError(t, err, 403, "InvalidSignatureException")

	r = signedRequest(`{}`, "AKIDOTHER", testSecret, testNow, false)
	_, err = a.Authenticate(r)
	wantAuthError(t, err, 403, "UnrecognizedClientException")

	// Wrong secret: detected by the body check (no hash header) or at once.
	r = signedRequest(`{}`, testKeyID, "wrong", testNow, false)
	check, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	wantAuthError(t, check([]byte(`{}`)), 403, "InvalidSignatureException")
	r = signedRequest(`{}`, testKeyID, "wrong", testNow, true)
	_, err = a.Authenticate(r)
	wantAuthError(t, err, 403, "InvalidSignatureException")

	// A signed header changed after signing.
	r = signedRequest(`{}`, testKeyID, testSecret, testNow, true)
	r.Host = "other:8080"
	_, err = a.Authenticate(r)
	wantAuthError(t, err, 403, "InvalidSignatureException")

	r = signedRequest(`{}`, testKeyID, testSecret, testNow, false)
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	_, err = a.Authenticate(r)
	wantAuthError(t, err, 403, "IncompleteSignatureException")
}

func TestSigV4Malformed(t *testing.T) {
	a := both(t)
	good := signedRequest(`{}`, testKeyID, testSecret, testNow, false).Header.Get("Authorization")
	cases := map[string]string{
		"no fields":        "AWS4-HMAC-SHA256 garbage",
		"short credential": "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261106, SignedHeaders=host, Signature=" + strings.Repeat("a", 64),
		"bad signature":    strings.Replace(good, "Signature=", "Signature=zz", 1),
		"other service":    strings.Replace(good, "/lambda/", "/s3/", 1),
		"no host":          strings.Replace(good, "SignedHeaders=host;", "SignedHeaders=", 1),
		"unsorted":         strings.Replace(good, "host;x-amz-date", "x-amz-date;host", 1),
	}
	for name, header := range cases {
		r := signedRequest(`{}`, testKeyID, testSecret, testNow, false)
		r.Header.Set("Authorization", header)
		if _, err := a.Authenticate(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r := signedRequest(`{}`, testKeyID, testSecret, testNow, false)
	r.Header.Set("X-Amz-Date", "20261105T120000Z") // not the scope date
	_, err := a.Authenticate(r)
	wantAuthError(t, err, 403, "IncompleteSignatureException")

	for name, mutate := range map[string]func(*http.Request){
		"bad x-amz-date": func(r *http.Request) { r.Header.Set("X-Amz-Date", "yesterday") },
		"no date":        func(r *http.Request) { r.Header.Del("X-Amz-Date") },
		"unsigned date": func(r *http.Request) {
			r.Header.Del("X-Amz-Date")
			r.Header.Set("Date", testNow.Format(http.TimeFormat))
		},
		"unsigned x-amz-date": func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "SignedHeaders=host;x-amz-date", "SignedHeaders=host", 1))
		},
	} {
		r := signedRequest(`{}`, testKeyID, testSecret, testNow, false)
		mutate(r)
		_, err := a.Authenticate(r)
		wantAuthError(t, err, 403, "IncompleteSignatureException")
		_ = name
	}
}

// TestSigV4WithDateHeader: a signature over the Date header instead of
// X-Amz-Date (older clients).
func TestSigV4WithDateHeader(t *testing.T) {
	a := both(t)
	r := httptest.NewRequest("POST", invokeURL, strings.NewReader(`{}`))
	signWith(r, []byte(`{}`), testKeyID, testSecret, "eu-west-3", testNow, signOptions{dateHeader: true})
	check, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := check([]byte(`{}`)); err != nil {
		t.Errorf("body check: %v", err)
	}
}

func TestAuthenticateLeavesHeadersAlone(t *testing.T) {
	a := both(t)
	r := signedRequest(`{}`, testKeyID, testSecret, testNow, false)
	r.Header.Set("X-Custom", "a   b")
	r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "host;x-amz-date", "host;x-amz-date;x-custom", 1))
	_, _ = a.Authenticate(r)
	if got := r.Header.Get("X-Custom"); got != "a   b" {
		t.Errorf("header modified: %q", got)
	}
}

func TestBearer(t *testing.T) {
	a := both(t)
	r := httptest.NewRequest("POST", invokeURL, nil)
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("no credential accepted")
	} else {
		wantAuthError(t, err, 401, "MissingAuthenticationTokenException")
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	if check, err := a.Authenticate(r); err != nil || check != nil {
		t.Errorf("valid token: %v", err)
	}
	r.Header.Set("Authorization", "Bearer "+testToken+"x")
	_, err := a.Authenticate(r)
	wantAuthError(t, err, 403, "UnrecognizedClientException")
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	_, err = a.Authenticate(r)
	wantAuthError(t, err, 403, "IncompleteSignatureException")
}

func TestMethodNotConfigured(t *testing.T) {
	onlyKey := newAuth(t, config.Auth{Enabled: true, AccessKey: config.Secret(testKeyID + ":" + testSecret)})
	r := httptest.NewRequest("POST", invokeURL, nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	_, err := onlyKey.Authenticate(r)
	wantAuthError(t, err, 403, "UnrecognizedClientException")

	onlyToken := newAuth(t, config.Auth{Enabled: true, Token: testToken})
	_, err = onlyToken.Authenticate(signedRequest(`{}`, testKeyID, testSecret, testNow, false))
	wantAuthError(t, err, 403, "UnrecognizedClientException")
}

func TestStoreFilesAndReload(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	keyFile := filepath.Join(dir, "key")
	write := func(path, v string) {
		if err := os.WriteFile(path, []byte(v), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	write(tokenFile, testToken+"\n")
	write(keyFile, testKeyID+":"+testSecret+"\n")
	s, err := NewStore(config.Auth{Enabled: true, TokenFile: tokenFile, AccessKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	c := s.Current()
	if c.TokenSHA256 != sha256.Sum256([]byte(testToken)) || !c.HasToken || c.AccessKeyID != testKeyID || c.SecretAccessKey != testSecret {
		t.Errorf("credentials = %+v", c)
	}
	// Rotation: new token on reload.
	_ = os.Remove(tokenFile)
	write(tokenFile, strings.Repeat("z", 40))
	if err := s.Reload(); err != nil || s.Current().TokenSHA256 != sha256.Sum256([]byte(strings.Repeat("z", 40))) {
		t.Errorf("reload: %v %+v", err, s.Current())
	}
	// A broken file keeps the credentials in force.
	_ = os.Remove(tokenFile)
	write(tokenFile, "short")
	if err := s.Reload(); err == nil || s.Current().TokenSHA256 != sha256.Sum256([]byte(strings.Repeat("z", 40))) {
		t.Errorf("broken reload: %v %+v", err, s.Current())
	}
}

func TestStoreErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	big := filepath.Join(dir, "big")
	_ = os.WriteFile(empty, []byte("  \n"), 0o400)
	_ = os.WriteFile(big, []byte(strings.Repeat("x", maxCredentialFile+1)), 0o400)
	for name, cfg := range map[string]config.Auth{
		"missing file": {TokenFile: filepath.Join(dir, "nope")},
		"directory":    {TokenFile: dir},
		"empty file":   {TokenFile: empty},
		"big file":     {AccessKeyFile: big},
		"bad key":      {AccessKey: "nocolon"},
		"nothing":      {},
	} {
		if _, err := NewStore(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestURIEncode(t *testing.T) {
	cases := map[string]string{
		"/2015-03-31/functions/my%3Af/invocations": "/2015-03-31/functions/my%253Af/invocations",
		"a b+c~d": "a%20b%2Bc~d",
		"é":       "%C3%A9",
		"/x/":     "/x/",
	}
	for in, want := range cases {
		if got := uriEncode(in, false); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := uriEncode("a/b", true); got != "a%2Fb" {
		t.Errorf("slash = %q", got)
	}
}

func TestUnsignedAmzHeadersRefused(t *testing.T) {
	a := both(t)
	for _, h := range []string{"X-Amz-Log-Type", "X-Amz-Client-Context", "X-Amz-Tenant-Id", "Content-Type"} {
		r := signedRequest(`{}`, testKeyID, testSecret, testNow, false)
		r.Header.Set(h, "Tail") // added after signing
		_, err := a.Authenticate(r)
		wantAuthError(t, err, 403, "IncompleteSignatureException")
	}
	// Signed, they pass.
	r := httptest.NewRequest("POST", invokeURL, strings.NewReader(`{}`))
	r.Header.Set("X-Amz-Log-Type", "Tail")
	r.Header.Set("Content-Type", "application/json")
	Sign(r, []byte(`{}`), testKeyID, testSecret, "eu-west-3", testNow)
	check, err := a.Authenticate(r)
	if err != nil || check([]byte(`{}`)) != nil {
		t.Errorf("signed headers: %v", err)
	}
	// The unsigned content hash header is refused too.
	r = signedRequest(`{}`, testKeyID, testSecret, testNow, false)
	r.Header.Set("X-Amz-Content-Sha256", sha256Hex([]byte(`{}`)))
	_, err = a.Authenticate(r)
	wantAuthError(t, err, 403, "IncompleteSignatureException")
}

func TestCanonicalQueryOrder(t *testing.T) {
	// Sorted by key first: "a" before "a-b", whatever the values.
	if got := canonicalQuery("a-b=1&a=2&b=x%20y&a=1"); got != "a=1&a=2&a-b=1&b=x%20y" {
		t.Errorf("canonical query = %q", got)
	}
}

func TestBearerByHash(t *testing.T) {
	sum := sha256.Sum256([]byte(testToken))
	dir := t.TempDir()
	hashFile := filepath.Join(dir, "token.sha256")
	if err := os.WriteFile(hashFile, []byte(hex.EncodeToString(sum[:])+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]config.Auth{
		"env":  {Enabled: true, TokenSHA256: hex.EncodeToString(sum[:])},
		"file": {Enabled: true, TokenSHA256File: hashFile},
	} {
		a := newAuth(t, cfg)
		r := httptest.NewRequest("POST", invokeURL, nil)
		r.Header.Set("Authorization", "Bearer "+testToken)
		if _, err := a.Authenticate(r); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		r.Header.Set("Authorization", "Bearer "+hex.EncodeToString(sum[:])) // the hash is not the token
		_, err := a.Authenticate(r)
		wantAuthError(t, err, 403, "UnrecognizedClientException")
	}
	bad := filepath.Join(dir, "bad")
	_ = os.WriteFile(bad, []byte("nothex"), 0o400)
	if _, err := NewStore(config.Auth{TokenSHA256File: bad}); err == nil {
		t.Error("invalid hash file accepted")
	}
}
