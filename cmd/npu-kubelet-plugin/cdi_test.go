package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/RBLN-SW/k8s-dra-driver-npu/internal/logtest"
	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/consts"
)

const runtimeSpecFixture = `kind: rebellions.ai/npu
devices:
- name: runtime
  containerEdits:
    mounts:
    - hostPath: /usr/lib/rbln
      containerPath: /usr/lib/rbln
      options: [ro, nosuid]
      type: bind
    hooks:
    - hookname: createContainer
      path: /usr/bin/rbln-ctk
      args: [rbln-ctk, hook]
`

func cdiHandlerWithRuntimeSpec(t *testing.T) *CDIHandler {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, rblnRuntimeSpecFile), []byte(runtimeSpecFixture), 0o600); err != nil {
		t.Fatalf("write runtime spec fixture: %v", err)
	}
	cdi, err := NewCDIHandler(root, consts.DriverName, "npu")
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
	cdi := cdiHandlerWithRuntimeSpec(t)

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
	cdi := cdiHandlerWithRuntimeSpec(t)

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
	cdi := cdiHandlerWithRuntimeSpec(t)

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
