package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Every other Rebellions component writes "ts" as an RFC3339 string. A float
// under the same key gives one field two JSON types in a shared index, so the
// JSON format carries the same RFC3339Nano string the text format does.
func TestJSONTimestampIsRFC3339Nano(t *testing.T) {
	before := time.Now()
	var buf bytes.Buffer
	mustLogger(t, &buf, "info", "json").Info("Started component")
	after := time.Now()

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	s, ok := m["ts"].(string)
	if !ok {
		t.Fatalf("ts = %#v, want RFC3339Nano string", m["ts"])
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("ts %q is not RFC3339Nano: %v", s, err)
	}
	if ts.Before(before) || ts.After(after) {
		t.Fatalf("ts = %v, outside [%v, %v]", ts, before, after)
	}
}

// The text format uses the same timestamp encoding as JSON.
func TestTextTimestampStaysRFC3339(t *testing.T) {
	var buf bytes.Buffer
	mustLogger(t, &buf, "info", "text").Info("Started component")
	out := buf.String()
	if !strings.Contains(out, "ts=") {
		t.Fatalf("text output missing ts: %s", out)
	}
	if !strings.Contains(out, "T") || !strings.Contains(out, ":") {
		t.Fatalf("text ts not RFC3339: %s", out)
	}
}

// Bucketing level to a closed vocabulary loses klog's V depth, and trace
// admits V(8) — far too much to read without filtering. Carrying klog's own
// `v` int alongside `level` restores the filter. Depth 0 says nothing that
// `level` does not already say, so info records carry no `v`.
func TestVerbosityAttrMirrorsKlogDepth(t *testing.T) {
	for _, tc := range []struct {
		emit  string
		level slog.Level
		wantV any // nil means the key must be absent
	}{
		{"error", slog.LevelError, nil},
		{"warn", slog.LevelWarn, nil},
		{"info", slog.LevelInfo, nil},
		{"debug", slog.LevelDebug, float64(4)},
		{"trace", LevelTrace, float64(8)},
	} {
		var buf bytes.Buffer
		logger := mustLogger(t, &buf, "trace", "json")
		logger.Log(context.Background(), tc.level, "Record")

		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("%s: not JSON: %v: %s", tc.emit, err, buf.String())
		}
		got, present := m["v"]
		if tc.wantV == nil {
			if present {
				t.Errorf("%s: v = %v, want absent", tc.emit, got)
			}
			continue
		}
		if !present {
			t.Errorf("%s: v absent, want %v", tc.emit, tc.wantV)
			continue
		}
		if got != tc.wantV {
			t.Errorf("%s: v = %v, want %v", tc.emit, got, tc.wantV)
		}
		// level must still be there: v alone is not the contract.
		if m["level"] != levelName(tc.level) {
			t.Errorf("%s: level = %v, want %q", tc.emit, m["level"], levelName(tc.level))
		}
	}
}
