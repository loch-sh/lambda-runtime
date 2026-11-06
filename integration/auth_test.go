// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/invokecli"
)

const (
	testToken     = "0123456789abcdef0123456789abcdef"
	testKeyID     = "LOCHTESTKEY000000001"
	testKeySecret = "c2VjcmV0LWtleS1mb3ItdGVzdHMtb25seS0wMTIzNDU2"
)

// tokenSHA256 is how the panel configures the bearer token: its hash only.
var tokenSHA256 = func() string {
	sum := sha256.Sum256([]byte(testToken))
	return hex.EncodeToString(sum[:])
}()

var authEnv = []string{
	"LOCH_RUNTIME_TOKEN_SHA256=" + tokenSHA256,
	"LOCH_RUNTIME_ACCESS_KEY=" + testKeyID + ":" + testKeySecret,
}

// awsCLIImage is pinned; LOCH_IT_AWS_CLI_IMAGE overrides it.
var awsCLIImage = cmp.Or(os.Getenv("LOCH_IT_AWS_CLI_IMAGE"), "mirror.gcr.io/amazon/aws-cli:2.39.5")

func (c *container) localInvoke(event string, args ...string) invokecli.Envelope {
	c.t.Helper()
	cmd := exec.Command("docker", append([]string{"exec", "-i", c.name, "/loch/lambda-runtime", "invoke"}, args...)...)
	cmd.Stdin = strings.NewReader(event)
	out, err := cmd.Output()
	if err != nil {
		c.t.Fatalf("invoke subcommand: %v %s", err, out)
	}
	var env invokecli.Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		c.t.Fatalf("envelope %q: %v", out, err)
	}
	return env
}

func TestAuthenticationAndLocalInvoke(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{auth: true, env: authEnv})
		c.waitHealth("ready")
		failuresBefore := c.metric(t, "loch_lambda_auth_failures_total")
		// Over the network: refused without the token, accepted with it.
		if r := c.invokeWith("POST", invokePath, []byte(`{}`), nil); r.status != 401 {
			t.Fatalf("no credential: %d %s", r.status, r.body)
		}
		if r := c.invokeWith("POST", invokePath, []byte(`{}`), http.Header{"Authorization": {"Bearer wrong-token-wrong-token-wrong-token"}}); r.status != 403 {
			t.Fatalf("wrong token: %d %s", r.status, r.body)
		}
		if r := c.invokeWith("POST", invokePath, []byte(`{}`), http.Header{"Authorization": {"Bearer " + testToken}}); r.status != 200 {
			t.Fatalf("bearer: %d %s", r.status, r.body)
		}
		// The invoke subcommand, as the panel runs it (docker exec).
		env := c.localInvoke(`{"hello":"exec"}`, "--log-tail")
		payload, _ := base64.StdEncoding.DecodeString(env.Payload)
		tail, _ := base64.StdEncoding.DecodeString(env.LogTail)
		if env.StatusCode != 200 || env.FunctionError != "" || env.RequestID == "" ||
			!strings.Contains(string(payload), "exec") || !strings.Contains(string(tail), "REPORT RequestId: "+env.RequestID) {
			t.Fatalf("envelope = %+v (%s)", env, payload)
		}
		env = c.localInvoke(`{"op":"throw"}`)
		if env.StatusCode != 200 || env.FunctionError != "Unhandled" || env.LogTail != "" {
			t.Fatalf("throw envelope = %+v", env)
		}
		// Metrics need the credential.
		if r := c.invokeWith("GET", "/_loch/metrics", nil, nil); r.status != 401 {
			t.Errorf("metrics without credential: %d", r.status)
		}
		// Three refusals above: no credential, wrong token, metrics without
		// credential.
		if got := c.metric(t, "loch_lambda_auth_failures_total"); got != failuresBefore+3 {
			t.Errorf("auth failures: %d -> %d", failuresBefore, got)
		}
		r := c.invokeWith("GET", "/_loch/metrics", nil, http.Header{"Authorization": {"Bearer " + testToken}})
		for _, want := range []string{`loch_lambda_invocations_total{outcome="success"} 2`, `loch_lambda_invocations_total{outcome="error"} 1`,
			`loch_lambda_state{state="ready"} 1`, "loch_lambda_memory_peak_bytes "} {
			if !strings.Contains(string(r.body), want) {
				t.Errorf("metrics lack %q:\n%s", want, r.body)
			}
		}
	})
}

// TestAWSCLIWithSigV4 invokes with the real aws-cli v2, endpoint-url and
// SigV4, from another container on the function's network.
func TestAWSCLIWithSigV4(t *testing.T) {
	netName := network(t)
	c := start(t, images[0], runOptions{auth: true, env: authEnv, network: netName, alias: "fn"})
	c.waitHealth("ready")
	cli := func(key string, extra ...string) (string, error) {
		args := []string{"run", "--rm", "--network", netName,
			"-e", "AWS_ACCESS_KEY_ID=" + testKeyID, "-e", "AWS_SECRET_ACCESS_KEY=" + key, "-e", "AWS_DEFAULT_REGION=eu-west-3",
			awsCLIImage, "lambda", "invoke", "--endpoint-url", "http://fn:8080",
			"--function-name", "function", "--payload", `{"via":"cli"}`, "--cli-binary-format", "raw-in-base64-out"}
		cmd := exec.Command("docker", append(append(args, extra...), "/dev/stdout")...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	out, err := cli(testKeySecret)
	if err != nil || !strings.Contains(out, `"via"`) || !strings.Contains(out, `"StatusCode": 200`) {
		t.Fatalf("aws lambda invoke: %v\n%s", err, out)
	}
	out, err = cli("wrong-secret")
	if err == nil || !strings.Contains(out, "InvalidSignatureException") {
		t.Fatalf("wrong secret: %v\n%s", err, out)
	}
	// --log-type Tail: the X-Amz-Log-Type header is signed by the CLI.
	out, err = cli(testKeySecret, "--log-type", "Tail")
	if err != nil || !strings.Contains(out, `"LogResult"`) {
		t.Fatalf("log tail: %v\n%s", err, out)
	}
}

// TestCredentialRotation: the token file is replaced, SIGHUP makes the
// runtime re-read it.
func TestCredentialRotation(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token.sha256")
	if err := os.WriteFile(tokenFile, []byte(tokenSHA256), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := start(t, images[0], runOptions{auth: true, mounts: []string{dir + ":/run/loch:ro"},
		env: []string{"LOCH_RUNTIME_TOKEN_SHA256_FILE=/run/loch/token.sha256"}})
	c.waitHealth("ready")
	bearer := func(token string) int {
		return c.invokeWith("POST", invokePath, []byte(`{}`), http.Header{"Authorization": {"Bearer " + token}}).status
	}
	if got := bearer(testToken); got != 200 {
		t.Fatalf("initial token: %d", got)
	}
	rotated := strings.Repeat("n", 40)
	rotatedSum := sha256.Sum256([]byte(rotated))
	// Rotate by renaming a complete file over the old one.
	if err := os.WriteFile(filepath.Join(dir, "token.new"), []byte(hex.EncodeToString(rotatedSum[:])), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "token.new"), tokenFile); err != nil {
		t.Fatal(err)
	}
	docker(t, "kill", "--signal", "HUP", c.name)
	deadline := time.Now().Add(10 * time.Second)
	for bearer(rotated) != 200 {
		if time.Now().After(deadline) {
			t.Fatal("rotated token not accepted")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := bearer(testToken); got != 403 {
		t.Errorf("old token after rotation: %d", got)
	}
}

// metric reads one unlabeled metric with the bearer token.
func (c *container) metric(t *testing.T, name string) int {
	t.Helper()
	r := c.invokeWith("GET", "/_loch/metrics", nil, http.Header{"Authorization": {"Bearer " + testToken}})
	for line := range strings.SplitSeq(string(r.body), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	t.Fatalf("metric %s not found:\n%s", name, r.body)
	return 0
}

// TestSDKv3FromAFunction: a Node.js function calls another function with
// the AWS SDK v3 bundled in the image (LambdaClient, endpoint, SigV4).
func TestSDKv3FromAFunction(t *testing.T) {
	netName := network(t)
	target := start(t, images[0], runOptions{auth: true, env: authEnv, network: netName, alias: "target"})
	caller := start(t, images[0], runOptions{network: netName, alias: "caller", env: []string{
		"TARGET_ENDPOINT=http://target:8080", "AWS_REGION=eu-west-3",
		"AWS_ACCESS_KEY_ID=" + testKeyID, "AWS_SECRET_ACCESS_KEY=" + testKeySecret,
		"AWS_EC2_METADATA_DISABLED=true", "AWS_LAMBDA_FUNCTION_TIMEOUT=20",
	}})
	target.waitHealth("ready")
	caller.waitHealth("ready")
	r := caller.invoke(`{"op":"sdk-invoke"}`)
	var out struct {
		StatusCode    int
		FunctionError *string
		Payload       struct{ Event map[string]string }
	}
	if r.status != 200 || r.functionError != "" || json.Unmarshal(r.body, &out) != nil ||
		out.StatusCode != 200 || out.FunctionError != nil || out.Payload.Event["via"] != "sdk" {
		t.Fatalf("SDK call: %s", r)
	}
}

// scanEnviron runs a function that searches /proc/*/environ for
// credentials while the health and invoke subcommands are run with docker
// exec, as the panel and the healthcheck do, and returns what it found.
func scanEnviron(t *testing.T, env []string) []string {
	t.Helper()
	c := start(t, images[0], runOptions{auth: true, env: env})
	c.waitHealth("ready")
	done := make(chan response, 1)
	go func() {
		event := []byte(`{"op":"scan-environ","ms":2000,"marker":"` + testToken + `"}`)
		r, _ := c.tryInvokeWith("POST", invokePath, event, http.Header{"Authorization": {"Bearer " + testToken}})
		done <- r
	}()
	execs := 0
	for deadline := time.Now().Add(1800 * time.Millisecond); time.Now().Before(deadline); execs++ {
		_, _ = c.dockerExec("/loch/lambda-runtime", "health")
		if execs%4 == 0 {
			// Queued behind the scan: its process lives during the scan.
			go func() {
				cmd := exec.Command("docker", "exec", "-i", c.name, "/loch/lambda-runtime", "invoke")
				cmd.Stdin = strings.NewReader(`{}`)
				_ = cmd.Run()
			}()
		}
	}
	r := <-done
	var out struct {
		Found []string
		Reads int
	}
	if r.status != 200 || json.Unmarshal(r.body, &out) != nil || out.Reads == 0 {
		t.Fatalf("scan: %s", r)
	}
	t.Logf("%d docker exec, %d environ reads, found %v", execs, out.Reads, out.Found)
	return out.Found
}

// TestNoSecretInExecProcesses: with the token given as its SHA-256 (and no
// SigV4 key), no process of the container, docker exec ones included,
// carries a secret the function could read.
func TestNoSecretInExecProcesses(t *testing.T) {
	if found := scanEnviron(t, []string{"LOCH_RUNTIME_TOKEN_SHA256=" + tokenSHA256}); len(found) != 0 {
		t.Errorf("secrets readable by the function: %v", found)
	}
}

// TestPlainTokenIsReadableThroughExec is the control of the scan: a plain
// LOCH_RUNTIME_TOKEN reaches the docker exec processes, where the function
// reads it (hence the SHA-256 variant).
func TestPlainTokenIsReadableThroughExec(t *testing.T) {
	if found := scanEnviron(t, []string{"LOCH_RUNTIME_TOKEN=" + testToken}); len(found) == 0 {
		t.Error("the scan did not find the plain token: it proves nothing")
	}
}

// invokeProcesses lists the invoke subcommands running in the container,
// seen from the host (docker top).
func (c *container) invokeProcesses(t *testing.T) int {
	t.Helper()
	n := 0
	for line := range strings.SplitSeq(docker(t, "top", c.name, "-o", "pid,args"), "\n") {
		if strings.Contains(line, "lambda-runtime invoke") {
			n++
		}
	}
	return n
}

// TestInvokeSubcommandEndsPromptly: the panel cannot kill a docker exec
// process; it closes the connection at its own deadline. The invoke
// subcommand must then end within a second, and always at its own bound.
func TestInvokeSubcommandEndsPromptly(t *testing.T) {
	c := start(t, images[0], runOptions{})
	c.waitHealth("ready")
	waitNoInvoke := func(what string, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for c.invokeProcesses(t) > 0 {
			if time.Now().After(deadline) {
				t.Fatalf("%s: an invoke process is still running after %s", what, within)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// 1. The caller goes away while the invocation runs. Docker does not
	// close the exec's stdout then, but it closes its stdin: with
	// --watch-stdin (stdin kept open after the event), invoke notices.
	cmd := exec.Command("docker", "exec", "-i", c.name, "/loch/lambda-runtime", "invoke", "--watch-stdin")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = stdin.Write([]byte(`{"op":"sleep","ms":2800}`)) // and stdin stays open
	time.Sleep(700 * time.Millisecond)
	if c.invokeProcesses(t) == 0 {
		t.Fatal("invoke not running")
	}
	_ = cmd.Process.Kill() // the caller disappears
	_ = cmd.Wait()
	waitNoInvoke("caller gone", time.Second)

	// 2. Its own deadline, shorter than the invocation (without
	// --watch-stdin, this is the bound of a caller that went away).
	time.Sleep(2500 * time.Millisecond) // let the previous invocation end
	start := time.Now()
	cmd = exec.Command("docker", "exec", "-i", c.name, "/loch/lambda-runtime", "invoke", "--timeout", "1")
	cmd.Stdin = strings.NewReader(`{"op":"sleep","ms":2800}`)
	out, err := cmd.CombinedOutput()
	if err == nil || time.Since(start) > 2500*time.Millisecond || !strings.Contains(string(out), "deadline") {
		t.Errorf("own deadline: %v after %s: %s", err, time.Since(start), out)
	}
	waitNoInvoke("own deadline", time.Second)

	// 3. stdin closed at once: an empty event, delivered.
	cmd = exec.Command("docker", "exec", "-i", c.name, "/loch/lambda-runtime", "invoke")
	cmd.Stdin = strings.NewReader("")
	if out, err := cmd.Output(); err != nil || !strings.Contains(string(out), `"statusCode":200`) {
		t.Errorf("empty stdin: %v %s", err, out)
	}
}
