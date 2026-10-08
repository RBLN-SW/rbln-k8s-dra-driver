package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/grpclog"
)

// grpc-go severities must land on the contract logger: INFO is connection
// lifecycle chatter (debug), WARNING/ERROR keep their severity, and every
// record carries "logger":"grpc" so grpc's free-form text is distinguishable
// from the driver's constant msgs.
func TestGrpcLoggerMapsSeverities(t *testing.T) {
	var buf bytes.Buffer
	g := grpcLogger{l: newLogger(&buf, slog.LevelDebug, "json").With("logger", "grpc")}

	g.Info("conn", "ected")
	g.Infoln("server", "created")
	g.Warningf("retry %d", 3)
	g.Errorln("boom")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %s", len(lines), buf.String())
	}
	want := []struct{ level, msg string }{
		{"debug", "connected"},
		{"debug", "server created"},
		{"warn", "retry 3"},
		{"error", "boom"},
	}
	for i, w := range want {
		var m map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &m); err != nil {
			t.Fatalf("line %d not JSON: %v: %s", i, err, lines[i])
		}
		if m["level"] != w.level || m["msg"] != w.msg {
			t.Errorf("line %d = level %v msg %q, want %s %q", i, m["level"], m["msg"], w.level, w.msg)
		}
		if m["logger"] != "grpc" {
			t.Errorf("line %d: logger = %v, want grpc", i, m["logger"])
		}
		if _, ok := m["ts"].(string); !ok {
			t.Errorf("line %d: ts = %#v, want RFC3339Nano string", i, m["ts"])
		}
	}
}

// At the default info gate grpc INFO must vanish entirely, and V must report
// verbosity disabled so grpc-go skips building those records at all.
func TestGrpcLoggerGatesInfoAndVerbosityAtInfo(t *testing.T) {
	var buf bytes.Buffer
	g := grpcLogger{l: newLogger(&buf, slog.LevelInfo, "json")}

	g.Info("noise")
	if out := buf.String(); out != "" {
		t.Fatalf("grpc INFO must be gated at info level: %s", out)
	}
	if g.V(2) {
		t.Fatal("V must be false when debug is gated")
	}

	dbg := grpcLogger{l: newLogger(&buf, slog.LevelDebug, "json")}
	if !dbg.V(2) {
		t.Fatal("V must be true when debug is enabled")
	}
}

// grpc-go's package-level calls must reach the contract logger once bridged;
// unbridged they go to a text logger that captured os.Stderr at init.
func TestBridgeGrpclogInstallsAdapter(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(newLogger(&buf, slog.LevelInfo, "json"))
	t.Cleanup(func() { slog.SetDefault(old) })

	BridgeGrpclog()
	grpclog.Warning("bridged grpc record")

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if m["level"] != "warn" || m["msg"] != "bridged grpc record" || m["logger"] != "grpc" {
		t.Errorf("record = %v, want warn/bridged grpc record/grpc", m)
	}
}
