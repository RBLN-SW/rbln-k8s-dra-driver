package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/RBLN-SW/k8s-dra-driver-npu/internal/logtest"
)

const runtimeSpecFixture = `cdiVersion: 0.6.0
kind: rebellions.ai/npu
devices:
- name: runtime
  containerEdits:
    mounts:
    - hostPath: /usr/lib/rbln
      containerPath: /usr/lib/rbln
      options: [ro, nosuid]
      type: bind
    hooks:
    - hookName: createContainer
      path: /usr/bin/rbln-ctk
      args: [rbln-ctk, hook]
`

func cdiHandlerWithRuntimeSpec(t *testing.T, fixture string) *CDIHandler {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, rblnRuntimeSpecFile), []byte(fixture), 0o600); err != nil {
		t.Fatalf("write runtime spec fixture: %v", err)
	}
	cdi, err := NewCDIHandler(root, "npu")
	if err != nil {
		t.Fatalf("CDI handler: %v", err)
	}
	return cdi
}

// CDI is the step between "the driver decided" and "the container can see the
// device". With no record of what was written, an operator debugging a
// container that has no /dev/rsd0 cannot tell whether the driver never wrote
// the spec or the runtime ignored it.
func TestCreateClaimSpecFileLogsInjectedDevices(t *testing.T) {
	buf := logtest.Capture(t, "debug")
	cdi := cdiHandlerWithRuntimeSpec(t, runtimeSpecFixture)

	devices := PreparedDevices{{
		Device: drapbv1.Device{DeviceName: "null", PoolName: "node-1"},
		ContainerEdits: &cdiapi.ContainerEdits{ContainerEdits: &cdispec.ContainerEdits{
			DeviceNodes: []*cdispec.DeviceNode{
				{Path: "/dev/rsd0", HostPath: "/dev/null"},
				{Path: "/dev/null", HostPath: "/dev/null"},
			},
		}},
	}}

	if err := cdi.CreateClaimSpecFile(context.Background(), "claim-uid-4", devices); err != nil {
		t.Fatalf("CreateClaimSpecFile: %v", err)
	}

	line := logtest.Find(logtest.Lines(t, buf), "Wrote CDI spec for claim")
	if line == nil {
		t.Fatalf("claim spec write was not logged: %s", buf.String())
	}
	if line["specName"] == nil {
		t.Error("specName missing: the operator needs the file to look at")
	}
	if line["deviceNodes"] == nil {
		t.Error("deviceNodes missing: which paths were injected is the whole question")
	}
}

func TestDeleteClaimSpecFileLogs(t *testing.T) {
	buf := logtest.Capture(t, "debug")
	cdi := cdiHandlerWithRuntimeSpec(t, runtimeSpecFixture)

	if err := cdi.CreateClaimSpecFile(context.Background(), "claim-uid-5", nil); err != nil {
		t.Fatalf("CreateClaimSpecFile: %v", err)
	}
	if err := cdi.DeleteClaimSpecFile(context.Background(), "claim-uid-5"); err != nil {
		t.Fatalf("DeleteClaimSpecFile: %v", err)
	}

	if logtest.Find(logtest.Lines(t, buf), "Removed CDI spec for claim") == nil {
		t.Fatalf("claim spec removal was not logged: %s", buf.String())
	}
}

// The common spec carries the UMD mounts and hooks the container toolkit laid
// down; "container cannot find the userspace library" starts here.
func TestCreateCommonSpecFileLogsRuntimeEdits(t *testing.T) {
	buf := logtest.Capture(t, "debug")
	cdi := cdiHandlerWithRuntimeSpec(t, runtimeSpecFixture)

	if err := cdi.CreateCommonSpecFile(context.Background()); err != nil {
		t.Fatalf("CreateCommonSpecFile: %v", err)
	}

	line := logtest.Find(logtest.Lines(t, buf), "Wrote common CDI spec with runtime edits")
	if line == nil {
		t.Fatalf("common spec write was not logged: %s", buf.String())
	}
	if line["mounts"] != float64(1) {
		t.Errorf("mounts = %v, want 1", line["mounts"])
	}
	if line["hooks"] != float64(1) {
		t.Errorf("hooks = %v, want 1", line["hooks"])
	}
}

func TestCommonSpecPreservesToolkitHookName(t *testing.T) {
	for _, key := range []string{"hookName", "hookname"} {
		t.Run(key, func(t *testing.T) {
			fixture := strings.Replace(runtimeSpecFixture, "hookName:", key+":", 1)
			cdi := cdiHandlerWithRuntimeSpec(t, fixture)
			if err := cdi.CreateCommonSpecFile(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Load the published spec as a new CDI consumer would.
			cache, err := cdiapi.NewCache(cdiapi.WithSpecDirs(cdi.root), cdiapi.WithAutoRefresh(false))
			if err != nil {
				t.Fatal(err)
			}
			device := cache.GetDevice(cdi.kind() + "=" + cdiCommonDeviceName)
			if device == nil || len(device.ContainerEdits.Hooks) != 1 {
				t.Fatalf("expected one hook in common CDI device, got %+v", device)
			}
			if name := device.ContainerEdits.Hooks[0].HookName; name != "createContainer" {
				t.Errorf("hook name = %q, want createContainer", name)
			}
		})
	}
}

// runtimeSpecMixedFixture carries edits at both levels, the way `rbln-ctk cdi
// generate` lays out rbln.yaml (spec-level UMD edits, deviceNodes on the device).
const runtimeSpecMixedFixture = `cdiVersion: 0.6.0
kind: rebellions.ai/npu
containerEdits:
  mounts:
  - hostPath: /usr/lib/rbln/librbln.so
    containerPath: /usr/lib/rbln/librbln.so
    options: [ro, nosuid]
    type: bind
  hooks:
  - hookName: createContainer
    path: /usr/bin/rbln-ctk
    args: [rbln-ctk, hook, spec-level]
devices:
- name: runtime
  containerEdits:
    deviceNodes:
    - path: /dev/rbln0
      hostPath: /dev/rbln0
    mounts:
    - hostPath: /usr/lib/rbln
      containerPath: /usr/lib/rbln
      options: [ro, nosuid]
      type: bind
    hooks:
    - hookName: createContainer
      path: /usr/bin/rbln-ctk
      args: [rbln-ctk, hook, device-level]
`

// The CLI layout keeps the UMD edits at spec level. The common device must
// carry them in CDI order (spec level first), with either hook name spelling,
// and must not pick up the CLI's device node.
func TestCommonSpecMergesSpecLevelEdits(t *testing.T) {
	for _, key := range []string{"hookName", "hookname"} {
		t.Run(key, func(t *testing.T) {
			fixture := strings.ReplaceAll(runtimeSpecMixedFixture, "hookName:", key+":")
			cdi := cdiHandlerWithRuntimeSpec(t, fixture)
			if err := cdi.CreateCommonSpecFile(context.Background()); err != nil {
				t.Fatal(err)
			}
			cache, err := cdiapi.NewCache(cdiapi.WithSpecDirs(cdi.root), cdiapi.WithAutoRefresh(false))
			if err != nil {
				t.Fatal(err)
			}
			device := cache.GetDevice(cdi.kind() + "=" + cdiCommonDeviceName)
			if device == nil {
				t.Fatal("common CDI device not published")
			}
			edits := device.ContainerEdits

			wantMounts := []string{"/usr/lib/rbln/librbln.so", "/usr/lib/rbln"}
			if len(edits.Mounts) != len(wantMounts) {
				t.Fatalf("got %d mounts, want %d: %+v", len(edits.Mounts), len(wantMounts), edits.Mounts)
			}
			for i, want := range wantMounts {
				if edits.Mounts[i].HostPath != want {
					t.Errorf("mount[%d] = %q, want %q", i, edits.Mounts[i].HostPath, want)
				}
			}

			wantHookArgs := []string{"spec-level", "device-level"}
			if len(edits.Hooks) != len(wantHookArgs) {
				t.Fatalf("got %d hooks, want %d: %+v", len(edits.Hooks), len(wantHookArgs), edits.Hooks)
			}
			for i, want := range wantHookArgs {
				if edits.Hooks[i].HookName != "createContainer" {
					t.Errorf("hook[%d] name = %q, want createContainer", i, edits.Hooks[i].HookName)
				}
				if args := edits.Hooks[i].Args; len(args) == 0 || args[len(args)-1] != want {
					t.Errorf("hook[%d] args = %v, want last arg %q", i, args, want)
				}
			}

			if len(edits.DeviceNodes) != 0 {
				t.Errorf("common device carries device nodes %+v, want none", edits.DeviceNodes)
			}
		})
	}
}

// Spec-level edits without a runtime entry is not a layout the toolkit writes;
// the plugin keeps reporting it instead of guessing.
func TestCommonSpecRequiresRuntimeDevice(t *testing.T) {
	specLevelOnly, _, _ := strings.Cut(runtimeSpecMixedFixture, "devices:")
	fixture := specLevelOnly + "devices: []\n"
	cdi := cdiHandlerWithRuntimeSpec(t, fixture)
	err := cdi.CreateCommonSpecFile(context.Background())
	if err == nil || !strings.Contains(err.Error(), `runtime device "runtime" not found`) {
		t.Fatalf("err = %v, want runtime device not found", err)
	}
}

// testRDSSpec mirrors the rbln-rds.yaml the RBLN container toolkit emits.
const testRDSSpec = `cdiVersion: "0.6.0"
kind: rebellions.ai/rds
devices:
  - name: rblnfs0
    containerEdits:
      deviceNodes:
        - path: /dev/rblnfs0
          hostPath: /dev/rblnfs0
          permissions: rw
  - name: all
    containerEdits:
      deviceNodes:
        - path: /dev/rblnfs0
          hostPath: /dev/rblnfs0
          permissions: rw
        - path: /dev/rblnfs1
          hostPath: /dev/rblnfs1
          permissions: rw
`

// testRDSSpecFromNode is the verbatim rbln-rds.yaml read off a live RDS node.
const testRDSSpecFromNode = `cdiVersion: 0.5.0
kind: rebellions.ai/rds
devices:
    - name: rblnfs0
      containerEdits:
        deviceNodes:
            - path: /dev/rblnfs0
              hostPath: /dev/rblnfs0
              permissions: rw
    - name: all
      containerEdits:
        deviceNodes:
            - path: /dev/rblnfs0
              hostPath: /dev/rblnfs0
              permissions: rw
`

func TestGetRDSDeviceNodes(t *testing.T) {
	t.Run("reads all nodes and preserves toolkit permissions", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, rblnRDSSpecFile), []byte(testRDSSpec), 0o600); err != nil {
			t.Fatalf("write spec: %v", err)
		}

		nodes, err := (&CDIHandler{root: root}).getRDSDeviceNodes()
		if err != nil {
			t.Fatalf("getRDSDeviceNodes: %v", err)
		}

		want := []string{"/dev/rblnfs0", "/dev/rblnfs1"}
		if len(nodes) != len(want) {
			t.Fatalf("got %d nodes, want %d: %+v", len(nodes), len(want), nodes)
		}
		for i, n := range nodes {
			if n.Path != want[i] || n.HostPath != want[i] {
				t.Errorf("node[%d] path/hostPath = %q/%q, want %q", i, n.Path, n.HostPath, want[i])
			}
			if n.Permissions != "rw" {
				t.Errorf("node[%d] permissions = %q, want %q (must match toolkit)", i, n.Permissions, "rw")
			}
		}
	})

	t.Run("parses the verbatim spec from a live node", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, rblnRDSSpecFile), []byte(testRDSSpecFromNode), 0o600); err != nil {
			t.Fatalf("write spec: %v", err)
		}

		nodes, err := (&CDIHandler{root: root}).getRDSDeviceNodes()
		if err != nil {
			t.Fatalf("getRDSDeviceNodes: %v", err)
		}
		if len(nodes) != 1 {
			t.Fatalf("got %d nodes, want 1: %+v", len(nodes), nodes)
		}
		if n := nodes[0]; n.Path != "/dev/rblnfs0" || n.HostPath != "/dev/rblnfs0" || n.Permissions != "rw" {
			t.Errorf("node = {Path:%q HostPath:%q Permissions:%q}, want /dev/rblnfs0 + rw", n.Path, n.HostPath, n.Permissions)
		}
	})

	t.Run("no spec file yields nothing", func(t *testing.T) {
		nodes, err := (&CDIHandler{root: t.TempDir()}).getRDSDeviceNodes()
		if err != nil {
			t.Fatalf("getRDSDeviceNodes: %v", err)
		}
		if nodes != nil {
			t.Fatalf("expected nil nodes on a host with no RDS spec, got %+v", nodes)
		}
	})
}

// vfio claims must not reference the common CDI device: it carries the RBLN
// runtime's UMD mounts and hooks, which a passthrough-only node does not have
// and a virt-launcher container must not receive.
func TestGetClaimDevicesIncludesCommonOnlyForNpu(t *testing.T) {
	cdi, err := NewCDIHandler(t.TempDir(), "npu")
	if err != nil {
		t.Fatalf("CDI handler: %v", err)
	}

	npu := cdi.GetClaimDevices("claim-uid", []string{"rbln0"}, true)
	if len(npu) != 2 || !strings.HasSuffix(npu[0], "="+cdiCommonDeviceName) {
		t.Errorf("npu claim devices = %v, want the common device first", npu)
	}

	vfio := cdi.GetClaimDevices("claim-uid", []string{"vfio-0000-27-00-0"}, false)
	if len(vfio) != 1 {
		t.Errorf("vfio claim devices = %v, want only the per-claim device", vfio)
	}
	for _, id := range vfio {
		if strings.Contains(id, "="+cdiCommonDeviceName) {
			t.Errorf("vfio claim devices = %v must not include the common device", vfio)
		}
	}
}
