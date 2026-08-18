package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func testClaim(annotations map[string]string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "virt-launcher-vm-npu-claim-abcde",
			UID:         "f3a8c2e1-7b4d-4e2a-9c1f-2d8e5a6b3c90",
			Annotations: annotations,
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Request: "npu",
							Driver:  "npu.rebellions.ai",
							Pool:    "worker-1",
							Device:  "vfio-0000-23-00-0",
						},
					},
				},
			},
		},
	}
}

func testAllocatable() AllocatableDevices {
	return AllocatableDevices{
		"vfio-0000-23-00-0": resourceapi.Device{
			Name: "vfio-0000-23-00-0",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				pciBusIDAttributeKey:   {StringValue: ptr.To("0000:23:00.0")},
				deviceTypeAttributeKey: {StringValue: ptr.To(deviceTypeVfio)},
			},
		},
	}
}

// Template-generated claims are addressed by the pod-level claim name that
// virt-launcher knows from vmi.spec.resourceClaims[].name.
func TestWriteKubeVirtMetadataTemplateClaim(t *testing.T) {
	base := t.TempDir()
	claim := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})

	dirs, err := writeKubeVirtMetadata(base, claim, "npu.rebellions.ai", testAllocatable())
	if err != nil {
		t.Fatalf("writeKubeVirtMetadata: %v", err)
	}
	wantDir := filepath.Join(base, "resourceclaimtemplates", "npu-claim", "npu")
	if len(dirs) != 1 || dirs[0] != wantDir {
		t.Fatalf("dirs = %v, want [%s]", dirs, wantDir)
	}

	// virt-launcher globs {dir}/*-metadata.json and requires the KEP-5304
	// apiVersion plus the standardized pciBusID attribute.
	data, err := os.ReadFile(filepath.Join(wantDir, "npu.rebellions.ai-metadata.json"))
	if err != nil {
		t.Fatalf("metadata file not written: %v", err)
	}
	var meta kubevirtDeviceMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.APIVersion != kubevirtMetadataAPIVersion {
		t.Fatalf("apiVersion = %q", meta.APIVersion)
	}
	if meta.PodClaimName == nil || *meta.PodClaimName != "npu-claim" {
		t.Fatalf("podClaimName = %v", meta.PodClaimName)
	}
	if len(meta.Requests) != 1 || meta.Requests[0].Name != "npu" ||
		len(meta.Requests[0].Devices) != 1 {
		t.Fatalf("requests = %+v", meta.Requests)
	}
	dev := meta.Requests[0].Devices[0]
	attr, ok := dev.Attributes[pciBusIDAttributeKey]
	if !ok || attr.StringValue == nil || *attr.StringValue != "0000:23:00.0" {
		t.Fatalf("pciBusID attribute missing or wrong: %+v", dev.Attributes)
	}
}

// Directly-created claims fall back to the claim object name.
func TestWriteKubeVirtMetadataDirectClaim(t *testing.T) {
	base := t.TempDir()
	claim := testClaim(nil)

	dirs, err := writeKubeVirtMetadata(base, claim, "npu.rebellions.ai", testAllocatable())
	if err != nil {
		t.Fatalf("writeKubeVirtMetadata: %v", err)
	}
	wantDir := filepath.Join(base, "resourceclaims", claim.Name, "npu")
	if len(dirs) != 1 || dirs[0] != wantDir {
		t.Fatalf("dirs = %v, want [%s]", dirs, wantDir)
	}
}

func TestRemoveKubeVirtMetadata(t *testing.T) {
	base := t.TempDir()
	claim := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})

	dirs, err := writeKubeVirtMetadata(base, claim, "npu.rebellions.ai", testAllocatable())
	if err != nil {
		t.Fatalf("writeKubeVirtMetadata: %v", err)
	}
	if err := removeKubeVirtMetadata(dirs); err != nil {
		t.Fatalf("removeKubeVirtMetadata: %v", err)
	}
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Fatalf("request dir still present: %v", err)
	}
	// The per-claim parent should be pruned when it became empty.
	if _, err := os.Stat(filepath.Dir(dirs[0])); !os.IsNotExist(err) {
		t.Fatalf("claim dir still present: %v", err)
	}
}
