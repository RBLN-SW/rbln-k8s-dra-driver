package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
