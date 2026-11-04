package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

func testClaim(annotations map[string]string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "team-a",
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
	mounts := kubevirtMetadataMounts(base, claim, "npu.rebellions.ai", "npu")
	if len(mounts) != 2 || mounts[0].ContainerPath != directPath || mounts[1].ContainerPath != templatePath {
		t.Fatalf("container lookup paths changed: %+v", mounts)
	}
	if mounts[0].HostPath != mounts[1].HostPath || mounts[0].HostPath == directPath || mounts[1].HostPath == templatePath {
		t.Fatal("both container layouts must share the UID-scoped host file")
	}

	// virt-launcher globs {dir}/*-metadata.json and requires the KEP-5304
	// apiVersion plus the standardized pciBusID attribute.
	data, err := os.ReadFile(mounts[0].HostPath)
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
	if meta.Kind != "DeviceMetadata" {
		t.Fatalf("kind changed: %q", meta.Kind)
	}
	if meta.PodClaimName == nil || *meta.PodClaimName != "npu-claim" {
		t.Fatalf("podClaimName = %v", meta.PodClaimName)
	}
	if len(meta.Requests) != 1 || meta.Requests[0].Name != "npu" ||
		len(meta.Requests[0].Devices) != 1 {
		t.Fatalf("requests = %+v", meta.Requests)
	}
	dev := meta.Requests[0].Devices[0]
	if dev.Driver != "npu.rebellions.ai" || dev.Pool != "worker-1" || dev.Name != "vfio-0000-23-00-0" {
		t.Fatalf("allocated resource names changed: %+v", dev)
	}
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

	mounts := kubevirtMetadataMounts(base, claim, "npu.rebellions.ai", kubevirtMetadataRequestName(&claim.Status.Allocation.Devices.Results[0]))
	if mounts[1].ContainerPath != metadataPath(base, "resourceclaimtemplates", "npu-claim", "npu") {
		t.Fatal("firstAvailable container path changed")
	}
	data, err := os.ReadFile(mounts[0].HostPath)
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
	mounts := kubevirtMetadataMounts(base, claim, "npu.rebellions.ai", "npu")
	if len(mounts) != 1 || mounts[0].ContainerPath != metadataPath(base, "resourceclaims", claim.Name, "npu") {
		t.Fatal("direct claim container path changed")
	}
	if _, err := os.Stat(mounts[0].HostPath); err != nil {
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
		kubevirtMetadataHostDir(base, string(claim.UID)),
		filepath.Join(base, "resourceclaimtemplates", "npu-claim"),
		filepath.Join(base, "resourceclaims", claim.Name),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("claim dir %s still present: %v", dir, err)
		}
	}
}

func TestKubeVirtMetadataClaimsWithSameAliasAreIsolated(t *testing.T) {
	base := t.TempDir()
	claimA := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})

	if err := writeKubeVirtMetadata(base, claimA, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatalf("writeKubeVirtMetadata(A): %v", err)
	}
	pathA := kubevirtMetadataMounts(base, claimA, "npu.rebellions.ai", "npu")[0].HostPath
	before, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}

	// Generated claims have distinct names and UIDs but the same pod alias.
	claimB := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})
	claimB.Name = "virt-launcher-another-vm-npu-claim-fghij"
	claimB.UID = "00000000-1111-2222-3333-444444444444"
	claimB.Status.Allocation.Devices.Results[0].Device = "vfio-0000-24-00-0"
	allocatedB := AllocatableDevices{"vfio-0000-24-00-0": {Name: "vfio-0000-24-00-0", Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		pciBusIDAttributeKey: {StringValue: ptr.To("0000:24:00.0")}, deviceTypeAttributeKey: {StringValue: ptr.To(deviceTypeVfio)},
	}}}
	if err := writeKubeVirtMetadata(base, claimB, "npu.rebellions.ai", allocatedB); err != nil {
		t.Fatal(err)
	}
	mountA := kubevirtMetadataMounts(base, claimA, "npu.rebellions.ai", "npu")[1]
	mountB := kubevirtMetadataMounts(base, claimB, "npu.rebellions.ai", "npu")[1]
	if mountA.HostPath == mountB.HostPath || mountA.ContainerPath != mountB.ContainerPath {
		t.Fatal("aliases must resolve to isolated host files at the same container path")
	}

	// B's rollback/unprepare must leave A's metadata alone.
	if err := removeKubeVirtMetadataForClaim(base, string(claimB.UID)); err != nil {
		t.Fatalf("removeKubeVirtMetadataForClaim(B): %v", err)
	}
	if after, err := os.ReadFile(pathA); err != nil || string(after) != string(before) {
		t.Fatalf("claim A metadata changed after B cleanup: %v", err)
	}
	if _, err := os.Stat(mountB.HostPath); !os.IsNotExist(err) {
		t.Fatalf("B metadata was not removed: %v", err)
	}

	// Re-preparing the same claim (same UID) stays idempotent.
	if err := writeKubeVirtMetadata(base, claimA, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatalf("idempotent rewrite for claim A failed: %v", err)
	}
}

func TestMetadataDirectClaimsWithSameNameAcrossNamespaces(t *testing.T) {
	base := t.TempDir()
	a := testClaim(nil)
	a.Name = "npu-claim"
	b := a.DeepCopy()
	b.Namespace = "team-b"
	b.UID = "00000000-1111-2222-3333-444444444444"
	for _, claim := range []*resourceapi.ResourceClaim{a, b} {
		if err := writeKubeVirtMetadata(base, claim, "npu.rebellions.ai", testAllocatable()); err != nil {
			t.Fatal(err)
		}
	}
	ma := kubevirtMetadataMounts(base, a, "npu.rebellions.ai", "npu")[0]
	mb := kubevirtMetadataMounts(base, b, "npu.rebellions.ai", "npu")[0]
	if ma.HostPath == mb.HostPath || ma.ContainerPath != mb.ContainerPath {
		t.Fatal("namespace-independent container names require UID-isolated host storage")
	}
	if err := removeKubeVirtMetadataForClaim(base, string(a.UID)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mb.HostPath); err != nil {
		t.Fatalf("other namespace's metadata was removed: %v", err)
	}
}

func TestMetadataOwnershipRejectsUnexpectedMarker(t *testing.T) {
	dir := kubevirtMetadataHostDir(t.TempDir(), "claim-a")
	if err := claimMetadataOwnership(dir, "claim-a"); err != nil {
		t.Fatal(err)
	}
	if err := claimMetadataOwnership(dir, "claim-b"); err == nil {
		t.Fatal("ownership checks must remain enforced")
	}
}

// Seed the old on-disk layout without using the new writer.
func legacyMetadataFixture(t *testing.T, base string, claim *resourceapi.ResourceClaim) []*cdispec.Mount {
	t.Helper()
	var mounts []*cdispec.Mount
	for _, dir := range []string{filepath.Join(base, "resourceclaims", claim.Name), filepath.Join(base, "resourceclaimtemplates", "npu-claim")} {
		if err := os.MkdirAll(filepath.Join(dir, "npu"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".claim-uid"), []byte(string(claim.UID)+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "npu", "npu.rebellions.ai-metadata.json")
		if err := os.WriteFile(file, []byte("legacy metadata"), 0644); err != nil {
			t.Fatal(err)
		}
		mounts = append(mounts, &cdispec.Mount{HostPath: file, ContainerPath: file, Options: []string{"ro", "bind"}})
	}
	return mounts
}

func TestMetadataCleanupHandlesLegacyAndUIDLayouts(t *testing.T) {
	base := t.TempDir()
	a := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})
	legacy := legacyMetadataFixture(t, base, a)
	if err := writeKubeVirtMetadata(base, a, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatal(err)
	}
	b := a.DeepCopy()
	b.UID = "00000000-1111-2222-3333-444444444444"
	if err := writeKubeVirtMetadata(base, b, "npu.rebellions.ai", testAllocatable()); err != nil {
		t.Fatal(err)
	}
	if err := removeKubeVirtMetadataForClaim(base, string(a.UID)); err != nil {
		t.Fatal(err)
	}
	for _, mount := range append(legacy, kubevirtMetadataMounts(base, a, "npu.rebellions.ai", "npu")...) {
		if _, err := os.Stat(mount.HostPath); !os.IsNotExist(err) {
			t.Fatalf("A metadata survived cleanup: %s, %v", mount.HostPath, err)
		}
	}
	if _, err := os.Stat(kubevirtMetadataMounts(base, b, "npu.rebellions.ai", "npu")[0].HostPath); err != nil {
		t.Fatalf("B metadata was removed: %v", err)
	}
}

func TestMetadataFailedWriteRollbackLeavesLegacyClaimIntact(t *testing.T) {
	base := t.TempDir()
	a := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})
	legacy := legacyMetadataFixture(t, base, a)
	b := a.DeepCopy()
	b.UID = "00000000-1111-2222-3333-444444444444"
	dir := kubevirtMetadataHostDir(base, string(b.UID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "npu"), []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeKubeVirtMetadata(base, b, "npu.rebellions.ai", testAllocatable()); err == nil {
		t.Fatal("expected metadata write failure")
	}
	if err := removeKubeVirtMetadataForClaim(base, string(b.UID)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("failed claim metadata survived rollback: %v", err)
	}
	for _, m := range legacy {
		data, err := os.ReadFile(m.HostPath)
		if err != nil || string(data) != "legacy metadata" {
			t.Fatalf("legacy claim changed: %v", err)
		}
	}
}

func TestPrepareReplaysLegacyMetadataMountsWithoutMigration(t *testing.T) {
	for _, directoryMount := range []bool{false, true} {
		name := "file mounts"
		if directoryMount {
			name = "directory mounts"
		}
		t.Run(name, func(t *testing.T) {
			st := newTestState(t)
			claim := testClaim(map[string]string{podClaimNameAnnotation: "npu-claim"})
			base := t.TempDir()
			mounts := legacyMetadataFixture(t, base, claim)
			if directoryMount {
				for _, m := range mounts {
					m.HostPath = filepath.Dir(filepath.Dir(m.HostPath))
					m.ContainerPath = m.HostPath
				}
			}
			devices := PreparedDevices{{Device: drapbv1.Device{RequestNames: []string{"npu"}, PoolName: "worker-1", DeviceName: "vfio-0000-23-00-0", CdiDeviceIds: []string{"k8s.npu.rebellions.ai/npu=legacy-device"}}, ContainerEdits: &cdiapi.ContainerEdits{ContainerEdits: &cdispec.ContainerEdits{Mounts: mounts}}}}
			cp := newCheckpoint()
			cp.V1.PreparedClaims[string(claim.UID)] = devices
			if err := st.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, cp); err != nil {
				t.Fatal(err)
			}
			// No allocatable inventory or CDI handler: replay must use the saved
			// result, not prepare again or move the files of a live claim.
			got, err := st.Prepare(context.Background(), claim)
			if err != nil || !reflect.DeepEqual(got, devices.GetDevices()) {
				t.Fatalf("legacy replay changed: %v, %v", got, err)
			}
			restored := newCheckpoint()
			if err := st.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, restored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(restored.V1.PreparedClaims[string(claim.UID)][0].ContainerEdits.Mounts, mounts) {
				t.Fatal("legacy mount paths changed")
			}
			if _, err := os.Stat(kubevirtMetadataHostDir(base, string(claim.UID))); !os.IsNotExist(err) {
				t.Fatal("checkpoint replay created new metadata storage")
			}
		})
	}
}

// Every mount source must be a regular file written by writeKubeVirtMetadata,
// never a directory.
func TestKubeVirtMetadataMountsUseWrittenFiles(t *testing.T) {
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
			mounts := kubevirtMetadataMounts(base, claim, "npu.rebellions.ai", kubevirtMetadataRequestName(result))
			if len(mounts) != tc.wantFiles {
				t.Fatalf("mounts = %v, want %d entries", mounts, tc.wantFiles)
			}
			for _, mount := range mounts {
				file := mount.HostPath
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
				if !reflect.DeepEqual(mount.Options, []string{"ro", "bind"}) {
					t.Fatalf("mount options changed: %v", mount.Options)
				}
			}
		})
	}
}
