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
