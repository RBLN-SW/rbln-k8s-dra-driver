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

func metadataPath(base string, segments ...string) string {
	return filepath.Join(append([]string{base}, append(segments, "npu.rebellions.ai-metadata.json")...)...)
}

// Template-generated claims must be resolvable through BOTH layouts: KubeVirt
// picks the directory by how the consuming pod references the claim, which
// may be a direct resourceClaimName reference to the generated claim.
func TestWriteKubeVirtMetadataTemplateClaim(t *testing.T) {
	base := t.TempDir()
	claim := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})

	if err := writeKubeVirtMetadata(base, claim, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatalf("writeKubeVirtMetadata: %v", err)
	}

	templatePath := metadataPath(base, "resourceclaimtemplates", "npu-claim", "npu")
	directPath := metadataPath(base, "resourceclaims", claim.Name, "npu")
	for _, path := range []string{templatePath, directPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("metadata file missing at %s: %v", path, err)
		}
	}

	// virt-launcher globs {dir}/*-metadata.json and requires the KEP-5304
	// apiVersion plus the standardized pciBusID attribute.
	data, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
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

// A firstAvailable subrequest allocates as "<mainRequest>/<subRequest>"; the
// metadata must land in the main-request directory virt-launcher globs.
func TestWriteKubeVirtMetadataFirstAvailableSubrequest(t *testing.T) {
	base := t.TempDir()
	claim := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})
	claim.Status.Allocation.Devices.Results[0].Request = "npu/ca25"

	if err := writeKubeVirtMetadata(base, claim, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatalf("writeKubeVirtMetadata: %v", err)
	}

	data, err := os.ReadFile(metadataPath(base, "resourceclaimtemplates", "npu-claim", "npu"))
	if err != nil {
		t.Fatalf("metadata not at main-request path: %v", err)
	}
	var meta kubevirtDeviceMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(meta.Requests) != 1 || meta.Requests[0].Name != "npu" {
		t.Fatalf("requests = %+v, want single request named 'npu'", meta.Requests)
	}
}

// Directly-created claims have no pod-claim-name annotation and use only the
// claim-name layout.
func TestWriteKubeVirtMetadataDirectClaim(t *testing.T) {
	base := t.TempDir()
	claim := testClaim(nil)

	if err := writeKubeVirtMetadata(base, claim, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatalf("writeKubeVirtMetadata: %v", err)
	}
	if _, err := os.Stat(metadataPath(base, "resourceclaims", claim.Name, "npu")); err != nil {
		t.Fatalf("metadata file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "resourceclaimtemplates")); !os.IsNotExist(err) {
		t.Fatalf("template layout should not exist for a direct claim: %v", err)
	}
}

func TestRemoveKubeVirtMetadataForClaim(t *testing.T) {
	base := t.TempDir()
	claim := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})

	if err := writeKubeVirtMetadata(base, claim, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatalf("writeKubeVirtMetadata: %v", err)
	}
	if err := removeKubeVirtMetadataForClaim(base, string(claim.UID)); err != nil {
		t.Fatalf("removeKubeVirtMetadataForClaim: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(base, "resourceclaimtemplates", "npu-claim"),
		filepath.Join(base, "resourceclaims", claim.Name),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("claim dir %s still present: %v", dir, err)
		}
	}
}

// Two claims sharing a pod-claim name must not silently corrupt each other:
// the second Prepare fails, and removal by a non-owner is a no-op.
func TestKubeVirtMetadataOwnership(t *testing.T) {
	base := t.TempDir()
	claimA := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})

	if err := writeKubeVirtMetadata(base, claimA, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatalf("writeKubeVirtMetadata(A): %v", err)
	}
	pathA := metadataPath(base, "resourceclaimtemplates", "npu-claim", "npu")

	// Same name, different claim UID: must be rejected, not overwritten.
	claimB := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})
	claimB.UID = "00000000-1111-2222-3333-444444444444"
	if err := writeKubeVirtMetadata(base, claimB, "npu.rebellions.ai", testAllocatable()); err == nil {
		t.Fatal("expected collision error for claim B, got nil")
	}

	// Removal keyed to the non-owner UID (claim B's rollback) must leave
	// A's metadata alone.
	if err := removeKubeVirtMetadataForClaim(base, string(claimB.UID)); err != nil {
		t.Fatalf("removeKubeVirtMetadataForClaim(B): %v", err)
	}
	if _, err := os.Stat(pathA); err != nil {
		t.Fatalf("claim A metadata was removed by non-owner: %v", err)
	}

	// Re-preparing the same claim (same UID) stays idempotent.
	if err := writeKubeVirtMetadata(base, claimA, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatalf("idempotent rewrite for claim A failed: %v", err)
	}
}

// The mount sources must be exactly the regular files writeKubeVirtMetadata
// wrote, one per layout, never a directory.
func TestKubeVirtMetadataFilesAreTheWrittenFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		annotations map[string]string
		request     string
		wantFiles   int
	}{
		"template claim, both layouts": {
			annotations: map[string]string{podClaimNameAnnotation: "npu-claim"},
			request:     "npu",
			wantFiles:   2,
		},
		"firstAvailable subrequest maps to the main request": {
			annotations: map[string]string{podClaimNameAnnotation: "npu-claim"},
			request:     "npu/ca25",
			wantFiles:   2,
		},
		"direct claim, claim-name layout only": {
			annotations: nil,
			request:     "npu",
			wantFiles:   1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			claim := testClaim(tc.annotations)
			claim.Status.Allocation.Devices.Results[0].Request = tc.request
			if err := writeKubeVirtMetadata(base, claim, "npu.rebellions.ai", testAllocatable()); err != nil {
				t.Fatalf("writeKubeVirtMetadata: %v", err)
			}

			result := &claim.Status.Allocation.Devices.Results[0]
			files := kubevirtMetadataFiles(base, claim, "npu.rebellions.ai", kubevirtMetadataRequestName(result))
			if len(files) != tc.wantFiles {
				t.Fatalf("kubevirtMetadataFiles = %v, want %d entries", files, tc.wantFiles)
			}
			for _, file := range files {
				fi, err := os.Stat(file)
				if err != nil {
					t.Fatalf("mount source %s was not written: %v", file, err)
				}
				if !fi.Mode().IsRegular() {
					t.Fatalf("mount source %s is not a regular file (mode %v)", file, fi.Mode())
				}
				if filepath.Base(filepath.Dir(file)) != "npu" {
					t.Fatalf("mount source %s is not under the main-request directory", file)
				}
			}
		})
	}
}
