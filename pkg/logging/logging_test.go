package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

// mustLogger builds a logger from string settings, failing the test on
// invalid input — the test-side equivalent of a validated production setup.
func mustLogger(t *testing.T, w io.Writer, level, format string) *slog.Logger {
	t.Helper()
	lvl, err := parseLevel(level)
	if err != nil {
		t.Fatalf("parseLevel(%q): %v", level, err)
	}
	f, err := parseFormat(format)
	if err != nil {
		t.Fatalf("parseFormat(%q): %v", format, err)
	}
	return newLogger(w, lvl, f)
}

func logLine(t *testing.T, level, format, emit string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := mustLogger(t, &buf, level, format)
	switch emit {
	case "info":
		logger.Info("Started component", "port", 8080)
	case "debug":
		logger.Debug("Polled daemon", "count", 3)
	case "trace":
		logger.Log(context.Background(), LevelTrace, "Dumped payload", "bytes", 42)
	}
	if buf.Len() == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	return m
}

func TestLoggerDefaultsToInfoJSONWithNormalizedKeys(t *testing.T) {
	m := logLine(t, "", "", "info")
	if m == nil {
		t.Fatal("info line suppressed at default level")
	}
	if m["level"] != "info" {
		t.Fatalf("level = %v, want info (lowercase)", m["level"])
	}
	if m["msg"] != "Started component" {
		t.Fatalf("msg = %v", m["msg"])
	}
	// Encoding is pinned by TestJSONTimestampMatchesKubeletEncoding; here we
	// only require the key to be present and numeric.
	if _, ok := m["ts"].(float64); !ok {
		t.Fatalf("ts missing or not a number: %#v", m["ts"])
	}
	if _, ok := m["caller"]; ok {
		t.Fatal("caller must be absent at info level")
	}
}

func TestLoggerGatesDebugAtInfo(t *testing.T) {
	if m := logLine(t, "info", "json", "debug"); m != nil {
		t.Fatalf("debug line leaked at info level: %v", m)
	}
}

func TestParseLevelAcceptsTrace(t *testing.T) {
	lvl, err := parseLevel("trace")
	if err != nil {
		t.Fatalf("parseLevel(trace): %v", err)
	}
	if lvl != LevelTrace {
		t.Fatalf("parseLevel(trace) = %v, want %v", lvl, LevelTrace)
	}
}

func TestParseRejectsUnknownLevelAndFormat(t *testing.T) {
	if _, err := parseLevel("loud"); err == nil {
		t.Fatal("want error for unknown level")
	}
	if _, err := parseFormat("yaml"); err == nil {
		t.Fatal("want error for unknown format")
	}
}

func TestTraceGatedAtDebug(t *testing.T) {
	if m := logLine(t, "debug", "json", "trace"); m != nil {
		t.Fatalf("trace line leaked at debug level: %v", m)
	}
	m := logLine(t, "trace", "json", "trace")
	if m == nil {
		t.Fatal("trace line suppressed at trace level")
	}
	if m["level"] != "trace" {
		t.Fatalf("level = %v, want trace", m["level"])
	}
}

// levelName must bucket every level — including the non-standard ones klog
// V(n) records arrive at through the logr bridge — into the closed
// error|warn|info|debug|trace vocabulary. This covers the mapping only; the
// end-to-end klog path is pinned by TestBridgeKlog* in klog_test.go.
func TestLevelBucketing(t *testing.T) {
	cases := []struct {
		in   slog.Level
		want string
	}{
		{slog.Level(-9), "trace"},
		{LevelTrace, "trace"},
		{slog.Level(-6), "trace"}, // klog V(6): kubeletplugin gRPC dumps
		{slog.Level(-5), "trace"}, // klog V(5)
		{slog.LevelDebug, "debug"},
		{slog.Level(-2), "debug"}, // klog V(2)
		{slog.Level(-1), "debug"}, // klog V(1)
		{slog.LevelInfo, "info"},
		{slog.Level(2), "info"},
		{slog.LevelWarn, "warn"},
		{slog.Level(7), "warn"},
		{slog.LevelError, "error"},
		{slog.Level(12), "error"},
	}
	for _, tc := range cases {
		if got := levelName(tc.in); got != tc.want {
			t.Errorf("levelName(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// The bucketed name must be what actually renders.
	var buf bytes.Buffer
	logger := mustLogger(t, &buf, "trace", "json")
	for _, tc := range cases {
		if tc.in < LevelTrace {
			continue // below the finest configurable gate; never rendered
		}
		buf.Reset()
		logger.Log(context.Background(), tc.in, "Bridged record")
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("not JSON: %v: %s", err, buf.String())
		}
		if m["level"] != tc.want {
			t.Errorf("rendered level(%v) = %v, want %q", tc.in, m["level"], tc.want)
		}
	}
}

func TestLoggerPassesThroughUserTimeAttr(t *testing.T) {
	var buf bytes.Buffer
	logger := mustLogger(t, &buf, "info", "json")
	logger.Info("Measured duration", "time", "1.5s")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if m["time"] != "1.5s" {
		t.Fatalf(`user "time" attr = %v, want "1.5s"`, m["time"])
	}
}

func TestLoggerTextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := mustLogger(t, &buf, "", "text")
	logger.Info("Started component", "port", 8080)
	out := buf.String()
	for _, want := range []string{"level=info", `msg="Started component"`, "ts=", "port=8080"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text output missing %q: %s", want, out)
		}
	}
}

// Both "warning" and the output spelling "warn" configure the warn gate;
// output always spells "warn".
func TestLoggerWarnLevelInputAliasesAndOutputSpelling(t *testing.T) {
	for _, level := range []string{"warning", "warn"} {
		var buf bytes.Buffer
		logger := mustLogger(t, &buf, level, "json")
		logger.Warn("Request failed")
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("not JSON: %v: %s", err, buf.String())
		}
		if m["level"] != "warn" {
			t.Fatalf("level(%q) = %v, want warn (output spelling)", level, m["level"])
		}
	}
}

func TestLoggerCallerPresentAtDebugGate(t *testing.T) {
	m := logLine(t, "debug", "json", "info")
	if m == nil {
		t.Fatal("info line suppressed at debug level")
	}
	caller, ok := m["caller"].(string)
	if !ok {
		t.Fatal("caller must be present at debug gate")
	}
	if !regexp.MustCompile(`^[^/]+/[^/]+\.go:\d+$`).MatchString(caller) {
		t.Fatalf("caller = %q, want dir/file.go:N", caller)
	}
}

func TestLoggerCallerPresentAtTraceGate(t *testing.T) {
	m := logLine(t, "trace", "json", "info")
	if m == nil {
		t.Fatal("info line suppressed at trace level")
	}
	if _, ok := m["caller"].(string); !ok {
		t.Fatal("caller must be present at trace gate (AddSource covers debug and below)")
	}
}

func TestTrimPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"c.go", "c.go"},
		{"b/c.go", "b/c.go"},
		{"a/b/c.go", "b/c.go"},
		{"x/a/b/c.go", "b/c.go"},
	} {
		if got := trimPath(tc.in); got != tc.want {
			t.Errorf("trimPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSetupFromEnvReturnsEffectiveSettings(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	for _, tc := range []struct {
		envLevel, envFormat   string
		wantLevel, wantFormat string
	}{
		{"", "", "info", "json"},
		{"trace", "text", "trace", "text"},
		{"bogus", "", "info", "json"},
	} {
		t.Setenv(envLogLevel, tc.envLevel)
		t.Setenv(envLogFormat, tc.envFormat)
		_, level, format := setupFromEnv(io.Discard)
		if level != tc.wantLevel || format != tc.wantFormat {
			t.Errorf("setupFromEnv(%q, %q) = (%q, %q), want (%q, %q)",
				tc.envLevel, tc.envFormat, level, format, tc.wantLevel, tc.wantFormat)
		}
	}
}

func TestSetupFromEnvFallsBack(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	t.Setenv(envLogLevel, "bogus")
	t.Setenv(envLogFormat, "json")
	level, format := SetupFromEnv()
	if level != "info" || format != "json" {
		t.Fatalf("SetupFromEnv() = (%q, %q), want (info, json)", level, format)
	}
	ctx := context.Background()
	if !slog.Default().Enabled(ctx, slog.LevelInfo) {
		t.Fatal("fallback logger must enable info")
	}
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Fatal("fallback logger must gate debug (info default)")
	}
}

func TestSetupFromEnvInvalidLevelEmitsWarn(t *testing.T) {
	t.Setenv(envLogLevel, "bogus")
	t.Setenv(envLogFormat, "json")
	var buf bytes.Buffer
	setupFromEnv(&buf)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("warn output not JSON: %v: %s", err, buf.String())
	}
	if m["msg"] != "Invalid "+envLogLevel+", using default" {
		t.Fatalf("msg = %v, want invalid-level warn", m["msg"])
	}
	if m["fallback"] != "info" {
		t.Fatalf("fallback = %v, want info", m["fallback"])
	}
}

func TestSetupFromEnvInvalidFormatFallsBackToJSON(t *testing.T) {
	t.Setenv(envLogLevel, "")
	t.Setenv(envLogFormat, "yaml")
	var buf bytes.Buffer
	logger, _, _ := setupFromEnv(&buf)
	// The warn itself must already be in the fallback format: JSON.
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("fallback output not JSON: %v: %s", err, buf.String())
	}
	if m["msg"] != "Invalid "+envLogFormat+", using default" {
		t.Fatalf("msg = %v, want invalid-format warn", m["msg"])
	}
	if m["fallback"] != "json" {
		t.Fatalf("fallback = %v, want json", m["fallback"])
	}
	buf.Reset()
	logger.Info("Started component")
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("subsequent records not JSON: %v: %s", err, buf.String())
	}
}
