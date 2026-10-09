package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/glog"
)

// lockedBuffer is a bytes.Buffer the relay goroutine can write while the test
// reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// bridgeGlogTo installs a buffer-backed contract logger at the given gate and
// bridges glog to it, restoring os.Stderr and the default logger afterwards.
func bridgeGlogTo(t *testing.T, gate string) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	old := slog.Default()
	slog.SetDefault(mustLogger(t, buf, gate, "json"))
	origStderr := os.Stderr
	BridgeGlog(gate)
	t.Cleanup(func() {
		os.Stderr = origStderr
		slog.SetDefault(old)
	})
	return buf
}

// waitForRecords polls until buf holds at least n JSON records. The relay is
// asynchronous, so a fixed sleep would be either flaky or slow.
func waitForRecords(t *testing.T, buf *lockedBuffer, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
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
		if len(out) >= n {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d records, want %d: %s", len(out), n, buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// rblnlib-go logs through glog, which has no sink API. Left alone it writes
// its own text format to stderr, which the container runtime interleaves with
// the JSON stream. Bridged, each glog line must become a contract record at
// glog's own severity, keeping the glog caller at the gates that show one.
func TestBridgeGlogRelaysRecordsAsContractJSON(t *testing.T) {
	buf := bridgeGlogTo(t, "debug")

	glog.Infof("Created RSD group %s for devices: %s", "3", "0000:17:00.0")
	glog.Warningf("Slow rbln-smi: %s", "2s")
	glog.Errorf("Failed to create RSD groups: %q", "boom")

	recs := waitForRecords(t, buf, 3)
	want := []struct{ level, msg string }{
		{"info", "Created RSD group 3 for devices: 0000:17:00.0"},
		{"warn", "Slow rbln-smi: 2s"},
		{"error", `Failed to create RSD groups: "boom"`},
	}
	for i, w := range want {
		m := recs[i]
		if m["level"] != w.level || m["msg"] != w.msg {
			t.Errorf("record %d = level %v msg %v, want %s %q", i, m["level"], m["msg"], w.level, w.msg)
		}
		if m["logger"] != "glog" {
			t.Errorf("record %d: logger = %v, want glog", i, m["logger"])
		}
		if _, ok := m["ts"].(string); !ok {
			t.Errorf("record %d: ts = %#v, want RFC3339Nano string", i, m["ts"])
		}
		// The caller is glog's, not the relay's: a handler-derived caller
		// would point at glog.go inside this package on every record.
		caller, _ := m["caller"].(string)
		if !regexp.MustCompile(`^glog_test\.go:\d+$`).MatchString(caller) {
			t.Errorf("record %d: caller = %q, want glog_test.go:N from the glog header", i, caller)
		}
	}
	if _, ok := recs[0]["v"]; ok {
		t.Errorf("info record carries v = %v, want absent", recs[0]["v"])
	}
}

// The contract shows callers at debug and below only; glog records follow it.
func TestBridgeGlogOmitsCallerAtInfoGate(t *testing.T) {
	buf := bridgeGlogTo(t, "info")

	glog.Infof("Destroyed RSD groups: %s", "3")

	m := waitForRecords(t, buf, 1)[0]
	if m["msg"] != "Destroyed RSD groups: 3" || m["logger"] != "glog" {
		t.Errorf("record = %v", m)
	}
	if caller, ok := m["caller"]; ok {
		t.Errorf("caller = %v, want absent at info gate", caller)
	}
}

// The gate applies to glog records exactly as to the process's own.
func TestBridgeGlogHonoursGate(t *testing.T) {
	buf := bridgeGlogTo(t, "error")

	glog.Infof("Dropped at error gate")
	glog.Errorf("Kept at error gate")

	// The pipe preserves order, so once the error record is visible the info
	// line before it has already been processed.
	recs := waitForRecords(t, buf, 1)
	if len(recs) != 1 || recs[0]["msg"] != "Kept at error gate" {
		t.Fatalf("records = %v, want only the error record", recs)
	}
}

// Whatever else reaches os.Stderr at write time is relayed too, at info,
// tagged so it can be told apart from glog and from the process's records.
func TestBridgeGlogRelaysRawStderrLines(t *testing.T) {
	buf := bridgeGlogTo(t, "info")

	fmt.Fprintln(os.Stderr, "raw stderr line")

	m := waitForRecords(t, buf, 1)[0]
	if m["level"] != "info" || m["msg"] != "raw stderr line" || m["logger"] != "stderr" {
		t.Errorf("record = %v, want info/raw stderr line/stderr", m)
	}
}
