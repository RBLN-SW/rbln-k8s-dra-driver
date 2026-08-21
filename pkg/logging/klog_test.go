package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"k8s.io/klog/v2"
)

// bridgeKlogTo installs a buffer-backed contract logger as the process default,
// bridges klog to it, and restores both gates afterwards.
func bridgeKlogTo(t *testing.T, level string) *bytes.Buffer {
	t.Helper()
	old := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(mustLogger(t, &buf, level, "json"))
	BridgeKlog(level)
	t.Cleanup(func() {
		BridgeKlog("info") // drop klog's own verbosity back to 0
		klog.ClearLogger()
		slog.SetDefault(old)
	})
	return &buf
}

// klog gates V(n) on its own verbosity before the slog handler is ever
// consulted, so SetSlogLogger alone silently drops every V(n>=1) record.
// Bridging must raise both gates to exactly the same depth: the deepest n the
// slog gate can render is -level (4 at debug, 8 at the trace floor).
func TestBridgeKlogRoutesVRecordsThroughContractGate(t *testing.T) {
	for _, tc := range []struct {
		gate      string
		v         int
		wantLevel string // "" means the record must be dropped
	}{
		{"info", 1, ""},
		{"debug", 1, "debug"},
		{"debug", 4, "debug"},
		{"debug", 5, ""},
		{"trace", 5, "trace"},
		{"trace", 6, "trace"},
		{"trace", 8, "trace"},
		{"trace", 9, ""},
	} {
		buf := bridgeKlogTo(t, tc.gate)
		klog.V(klog.Level(tc.v)).InfoS("Bridged V record")

		if tc.wantLevel == "" {
			if buf.Len() != 0 {
				t.Errorf("gate=%s V(%d): want dropped, got %s", tc.gate, tc.v, buf.String())
			}
			continue
		}
		if buf.Len() == 0 {
			t.Errorf("gate=%s V(%d): record dropped, want level %q", tc.gate, tc.v, tc.wantLevel)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Errorf("gate=%s V(%d): not JSON: %v: %s", tc.gate, tc.v, err, buf.String())
			continue
		}
		if m["level"] != tc.wantLevel {
			t.Errorf("gate=%s V(%d): level = %v, want %q", tc.gate, tc.v, m["level"], tc.wantLevel)
		}
		if m["msg"] != "Bridged V record" {
			t.Errorf("gate=%s V(%d): msg = %v", tc.gate, tc.v, m["msg"])
		}
	}
}

// Non-verbose klog records must reach the contract stream at every gate that
// admits their severity.
func TestBridgeKlogRoutesNonVerboseRecords(t *testing.T) {
	buf := bridgeKlogTo(t, "info")
	klog.InfoS("Plain klog record", "k", "v")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if m["level"] != "info" {
		t.Fatalf("level = %v, want info", m["level"])
	}
	if m["msg"] != "Plain klog record" {
		t.Fatalf("msg = %v", m["msg"])
	}
	if m["k"] != "v" {
		t.Fatalf("attr k = %v, want v", m["k"])
	}
}
