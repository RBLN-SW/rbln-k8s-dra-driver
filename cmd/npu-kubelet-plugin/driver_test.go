package main

import (
	"context"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	"github.com/RBLN-SW/k8s-dra-driver-npu/internal/logtest"
	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/consts"
)

// newTestState builds a DeviceState with only the checkpoint machinery wired.
// That is enough to exercise the failure paths that reject a claim before any
// device node is touched.
func newTestState(t *testing.T) *DeviceState {
	t.Helper()
	cm, err := checkpointmanager.NewCheckpointManager(t.TempDir())
	if err != nil {
		t.Fatalf("checkpoint manager: %v", err)
	}
	if err := cm.CreateCheckpoint(DriverPluginCheckpointFile, newCheckpoint()); err != nil {
		t.Fatalf("create checkpoint: %v", err)
	}
	return &DeviceState{driverName: consts.DriverName, checkpointManager: cm}
}

// A prepare failure is returned to kubelet but, until now, never logged by the
// driver: an operator tailing driver logs during a stuck ContainerCreating saw
// nothing. The record must also identify the claim the way an operator holds it
// (namespace/name), not only by UID, and must keep the requestID the
// kubeletplugin helper attached to the call context.
func TestPrepareResourceClaimLogsFailureWithClaimIdentityAndRequestID(t *testing.T) {
	buf := logtest.Capture(t, "info")

	d := &driver{state: newTestState(t)}
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "npu-claim", UID: "claim-uid-1"},
	}

	// What the helper's gRPC interceptor hands us.
	ctx := klog.NewContext(context.Background(),
		klog.LoggerWithValues(klog.FromContext(context.Background()), "requestID", 42))

	result := d.prepareResourceClaim(ctx, claim)
	if result.Err == nil {
		t.Fatal("want prepare error for an unallocated claim")
	}

	line := logtest.Find(logtest.Lines(t, buf), "Failed to prepare devices for claim")
	if line == nil {
		t.Fatalf("prepare failure was not logged: %s", buf.String())
	}
	if line["level"] != "error" {
		t.Errorf("level = %v, want error", line["level"])
	}
	if line["claimNamespace"] != "team-a" {
		t.Errorf("claimNamespace = %v, want team-a", line["claimNamespace"])
	}
	if line["claimName"] != "npu-claim" {
		t.Errorf("claimName = %v, want npu-claim", line["claimName"])
	}
	if line["claimUID"] != "claim-uid-1" {
		t.Errorf("claimUID = %v, want claim-uid-1", line["claimUID"])
	}
	if line["requestID"] != float64(42) {
		t.Errorf("requestID = %v, want 42 (helper context value)", line["requestID"])
	}
	if errStr, ok := line["err"].(string); !ok || !strings.Contains(errStr, "not yet allocated") {
		t.Errorf("err = %v, want it to mention the cause", line["err"])
	}
}

// The liveness probe calls NodePrepareResources with an empty claim list once
// per probe period, and the kubeletplugin helper forwards it here regardless.
// Logging that at info means ~8k records a node per day that are
// indistinguishable from real kubelet prepare traffic, which is precisely the
// signal an operator greps for.
func TestPrepareResourceClaimsDoesNotLogEmptyProbeBatches(t *testing.T) {
	buf := logtest.Capture(t, "info")

	d := &driver{state: newTestState(t)}
	if _, err := d.PrepareResourceClaims(context.Background(), nil); err != nil {
		t.Fatalf("PrepareResourceClaims(nil): %v", err)
	}
	if _, err := d.UnprepareResourceClaims(context.Background(), nil); err != nil {
		t.Fatalf("UnprepareResourceClaims(nil): %v", err)
	}

	if buf.Len() != 0 {
		t.Fatalf("empty probe batch produced records: %s", buf.String())
	}
}

// Unpreparing a claim the plugin has no record of is a legitimate no-op, but a
// silent one leaves an operator chasing leaked devices with no evidence either
// way.
func TestUnprepareResourceClaimLogsUnknownClaim(t *testing.T) {
	buf := logtest.Capture(t, "debug")

	d := &driver{state: newTestState(t)}
	claim := kubeletplugin.NamespacedObject{
		UID:            "claim-uid-2",
		NamespacedName: types.NamespacedName{Namespace: "team-b", Name: "gone-claim"},
	}

	if err := d.unprepareResourceClaim(context.Background(), claim); err != nil {
		t.Fatalf("unprepare of unknown claim: %v", err)
	}

	line := logtest.Find(logtest.Lines(t, buf), "No prepared state for claim, nothing to unprepare")
	if line == nil {
		t.Fatalf("unknown-claim no-op was not logged: %s", buf.String())
	}
	if line["claimUID"] != "claim-uid-2" {
		t.Errorf("claimUID = %v, want claim-uid-2", line["claimUID"])
	}
}
