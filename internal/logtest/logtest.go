// Package logtest captures contract-shaped log output for tests.
//
// It exists because getting this right by hand is easy to get wrong in two
// ways, both of which produce a passing test that proves nothing:
//
//   - Building a bare slog.JSONHandler instead of logging.New. The handler is
//     what lowercases and buckets "level", renames "time" to "ts" and shortens
//     "source" to "caller", so a test on the stdlib default asserts "ERROR"
//     and a schema the operator's pipeline never sees.
//   - Calling slog.SetDefault without logging.BridgeKlog. Records emitted
//     through logging.FromContext resolve via klog's global logger, so without
//     the bridge they go to klog's own text writer on stderr and the buffer
//     stays empty.
package logtest

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/klog/v2"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
)

// Capture installs a buffer-backed contract logger at the given level as the
// process default and bridges klog to it, so both slog.Default() and
// logging.FromContext records land in the returned buffer. Both gates are
// restored when the test ends.
func Capture(t *testing.T, level string) *bytes.Buffer {
	t.Helper()
	old := slog.Default()
	var buf bytes.Buffer
	logger, err := logging.New(&buf, level, "json")
	if err != nil {
		t.Fatalf("logging.New(%q): %v", level, err)
	}
	slog.SetDefault(logger)
	logging.BridgeKlog(level)
	t.Cleanup(func() {
		logging.BridgeKlog("info") // drop klog's global -v back to 0
		klog.ClearLogger()
		slog.SetDefault(old)
	})
	return &buf
}

// Lines decodes every captured record, failing the test on anything that is not
// a JSON object — a malformed line is itself a contract violation.
func Lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line not JSON: %v: %s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// Find returns the first record with the given msg, or nil.
func Find(lines []map[string]any, msg string) map[string]any {
	for _, m := range lines {
		if m["msg"] == msg {
			return m
		}
	}
	return nil
}
