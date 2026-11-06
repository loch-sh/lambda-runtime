// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Status is the outcome written in END and REPORT.
type Status string

// Statuses of the platform lines.
const (
	StatusSuccess Status = "success"
	StatusError   Status = "error"
	StatusTimeout Status = "timeout"
)

// Report holds the figures of a REPORT line.
type Report struct {
	RequestID string
	Duration  time.Duration
	// MaxMemoryUsedMB is 0 when unknown (no memory.peak).
	MaxMemoryUsedMB int64
	// InitDuration is set on the first report after an init.
	InitDuration time.Duration
	Status       Status
	ErrorType    string
}

// Platform formats the platform lines, as plain text (Lambda's default)
// or as JSON records (AWS_LAMBDA_LOG_FORMAT=JSON).
type Platform struct {
	JSON     bool
	Version  string
	MemoryMB int
	// Now defaults to time.Now.
	Now func() time.Time
}

// Start formats the START line.
func (p Platform) Start(requestID string) string {
	if p.JSON {
		return p.record("platform.start", map[string]any{"requestId": requestID, "version": p.Version})
	}
	return "START RequestId: " + requestID + " Version: " + p.Version
}

// End formats the END line (platform.runtimeDone in JSON).
func (p Platform) End(requestID string, status Status) string {
	if p.JSON {
		return p.record("platform.runtimeDone", map[string]any{"requestId": requestID, "status": status})
	}
	return "END RequestId: " + requestID
}

// Timeout formats the line Lambda writes when an invocation times out;
// "" in JSON, where the status of the records carries it.
func (p Platform) Timeout(requestID string, timeout time.Duration) string {
	if p.JSON {
		return ""
	}
	return fmt.Sprintf("%s %s Task timed out after %.2f seconds",
		p.now().UTC().Format("2006-01-02T15:04:05.000Z"), requestID, timeout.Seconds())
}

// Report formats the REPORT line: tab-separated fields, each followed by a
// tab, as Lambda writes them.
func (p Platform) Report(r Report) string {
	if p.JSON {
		return p.jsonReport(r)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "REPORT RequestId: %s\tDuration: %.2f ms\tBilled Duration: %d ms\tMemory Size: %d MB\t",
		r.RequestID, millis(r.Duration), billed(r.Duration), p.MemoryMB)
	if r.MaxMemoryUsedMB > 0 {
		fmt.Fprintf(&b, "Max Memory Used: %d MB\t", r.MaxMemoryUsedMB)
	}
	if r.InitDuration > 0 {
		fmt.Fprintf(&b, "Init Duration: %.2f ms\t", millis(r.InitDuration))
	}
	switch r.Status {
	case StatusTimeout:
		b.WriteString("Status: timeout\t")
	case StatusError:
		fmt.Fprintf(&b, "Status: error\tError Type: %s\t", r.ErrorType)
	}
	return b.String()
}

func (p Platform) jsonReport(r Report) string {
	metrics := map[string]any{
		"durationMs":       round2(millis(r.Duration)),
		"billedDurationMs": billed(r.Duration),
		"memorySizeMB":     p.MemoryMB,
	}
	if r.MaxMemoryUsedMB > 0 {
		metrics["maxMemoryUsedMB"] = r.MaxMemoryUsedMB
	}
	if r.InitDuration > 0 {
		metrics["initDurationMs"] = round2(millis(r.InitDuration))
	}
	record := map[string]any{"requestId": r.RequestID, "metrics": metrics, "status": r.Status}
	if r.Status == StatusError && r.ErrorType != "" {
		record["errorType"] = r.ErrorType
	}
	return p.record("platform.report", record)
}

func (p Platform) record(kind string, record map[string]any) string {
	b, _ := json.Marshal(struct {
		Time   string         `json:"time"`
		Type   string         `json:"type"`
		Record map[string]any `json:"record"`
	}{p.now().UTC().Format(time.RFC3339Nano), kind, record})
	return string(b)
}

func (p Platform) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// billed is the duration rounded up to the millisecond, at least 1 ms.
func billed(d time.Duration) int64 {
	return max(1, int64(math.Ceil(millis(d))))
}
