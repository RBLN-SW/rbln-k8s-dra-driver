package logging

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/klog/v2"
)

// The DRA kubeletplugin helper wraps every gRPC call with a context logger
// carrying requestID and method (kubeletplugin/nonblockinggrpcserver.go).
// Call sites that reach for slog.Default() throw that correlation away, so
// driver records cannot be tied to the kubelet call that caused them.
func TestFromContextKeepsHelperRequestValues(t *testing.T) {
	buf := bridgeKlogTo(t, "info")

	// Exactly what the helper does to the context it hands us.
	logger := klog.LoggerWithValues(klog.FromContext(context.Background()),
		"requestID", 7, "method", "/NodePrepareResources")
	ctx := klog.NewContext(context.Background(), logger)

	FromContext(ctx).Info("Prepared devices for claim", "claimUID", "abc")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if m["requestID"] != float64(7) {
		t.Errorf("requestID = %v, want 7", m["requestID"])
	}
	if m["method"] != "/NodePrepareResources" {
		t.Errorf("method = %v, want /NodePrepareResources", m["method"])
	}
	if m["claimUID"] != "abc" {
		t.Errorf("claimUID = %v, want abc", m["claimUID"])
	}
	if m["msg"] != "Prepared devices for claim" {
		t.Errorf("msg = %v", m["msg"])
	}
}

// WithValues is how a request path attaches its own correlation keys (claim
// identity) without every downstream function growing a logger parameter. It
// must inherit, not replace, what the caller's context already carries.
func TestWithValuesInheritsExistingContextValues(t *testing.T) {
	buf := bridgeKlogTo(t, "info")

	ctx := klog.NewContext(context.Background(),
		klog.LoggerWithValues(klog.FromContext(context.Background()), "requestID", 7))
	ctx = WithValues(ctx, "claimUID", "abc", "claimName", "npu-claim")

	FromContext(ctx).Info("Downstream record")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if m["requestID"] != float64(7) {
		t.Errorf("requestID = %v, want 7 (inherited)", m["requestID"])
	}
	if m["claimUID"] != "abc" {
		t.Errorf("claimUID = %v, want abc", m["claimUID"])
	}
	if m["claimName"] != "npu-claim" {
		t.Errorf("claimName = %v, want npu-claim", m["claimName"])
	}
}

// A context without a logger must still produce the contract logger, and the
// round-trip must not widen the gate.
func TestFromContextFallsBackAndKeepsGate(t *testing.T) {
	buf := bridgeKlogTo(t, "info")

	FromContext(context.Background()).Info("Info survives")
	if buf.Len() == 0 {
		t.Fatal("info record dropped on bare context")
	}

	buf.Reset()
	FromContext(context.Background()).Debug("Debug must be gated")
	if buf.Len() != 0 {
		t.Fatalf("debug leaked at info gate: %s", buf.String())
	}
}

// Records routed through the context logger keep the normalized contract keys,
// including the short caller at the debug gate.
func TestFromContextKeepsContractKeys(t *testing.T) {
	buf := bridgeKlogTo(t, "debug")
	FromContext(context.Background()).Debug("Contract keys intact")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if _, ok := m["ts"].(float64); !ok {
		t.Errorf("ts = %#v, want float epoch millis", m["ts"])
	}
	if m["level"] != "debug" {
		t.Errorf("level = %v, want debug", m["level"])
	}
	if m["v"] != float64(4) {
		t.Errorf("v = %v, want 4", m["v"])
	}
	caller, ok := m["caller"].(string)
	if !ok {
		t.Fatalf("caller missing: %v", m["caller"])
	}
	if want := "logging/context_test.go"; !strings.Contains(caller, want) {
		t.Errorf("caller = %q, want it to contain %q", caller, want)
	}
}
