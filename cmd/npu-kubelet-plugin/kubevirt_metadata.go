/*
 * Copyright 2026 Rebellions Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
)

// KubeVirt's virt-launcher discovers the PCI address of DRA-allocated host
// devices by reading KEP-5304 style metadata files from a node-local
// directory (see kubevirt pkg/dra/utils.go). KEP-5304 has not landed in the
// kubelet yet, so publishing these files is the DRA driver's job:
//
//	Template claims: {base}/resourceclaimtemplates/{podClaimName}/{request}/{driver}-metadata.json
//	Direct claims:   {base}/resourceclaims/{claimName}/{request}/{driver}-metadata.json
//
// The files are not mounted into virt-launcher by KubeVirt either; the
// prepare path injects a read-only CDI mount of each metadata file alongside
// the vfio device nodes.
const (
	kubevirtMetadataBasePath    = "/var/run/kubernetes.io/dra-device-attributes"
	kubevirtMetadataAPIVersion  = "metadata.resource.k8s.io/v1alpha1"
	kubevirtMetadataKind        = "DeviceMetadata"
	resourceClaimsSubdir        = "resourceclaims"
	resourceClaimTemplateSubdir = "resourceclaimtemplates"
	// The claim directory is keyed by a name that is NOT unique on the node
	// (pod-level claim name, or claim name without namespace) — that is
	// KubeVirt's contract and cannot be changed on the writer side. The
	// owner marker records which claim UID holds the directory so a
	// colliding Prepare fails loudly instead of silently corrupting, and
	// Unprepare never removes a directory another claim has taken over.
	// virt-launcher globs only {claimDir}/{request}/*-metadata.json, so the
	// marker at claim level is invisible to it.
	kubevirtMetadataOwnerFile = ".claim-uid"
	// Set by kube-controller-manager on claims generated from a
	// ResourceClaimTemplate; absent on directly-created claims.
	podClaimNameAnnotation = "resource.kubernetes.io/pod-claim-name"
)

// kubevirtDeviceMetadata mirrors kubevirt pkg/dra/metadata.DeviceMetadata
// (KEP-5304 v1alpha1). Field names must match its JSON expectations exactly.
type kubevirtDeviceMetadata struct {
	APIVersion   string                          `json:"apiVersion"`
	Kind         string                          `json:"kind"`
	PodClaimName *string                         `json:"podClaimName,omitempty"`
	Requests     []kubevirtDeviceMetadataRequest `json:"requests,omitempty"`
}

type kubevirtDeviceMetadataRequest struct {
	Name    string                   `json:"name"`
	Devices []kubevirtMetadataDevice `json:"devices,omitempty"`
}

type kubevirtMetadataDevice struct {
	Driver     string                                                    `json:"driver"`
	Pool       string                                                    `json:"pool"`
	Name       string                                                    `json:"name"`
	Attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute `json:"attributes,omitempty"`
}

// kubevirtMetadataClaimDirs returns every directory KubeVirt may resolve for
// this claim. virt-launcher picks the layout by how the consuming pod
// references the claim, not by how the claim was created: a template-generated
// claim consumed directly via resourceClaimName is looked up under
// resourceclaims/{claimName}. Template-generated claims therefore get both
// layouts (they always also carry a real claim name).
func kubevirtMetadataClaimDirs(basePath string, claim *resourceapi.ResourceClaim) []string {
	dirs := []string{filepath.Join(basePath, resourceClaimsSubdir, claim.Name)}
	if podClaimName, ok := claim.Annotations[podClaimNameAnnotation]; ok && podClaimName != "" {
		dirs = append(dirs, filepath.Join(basePath, resourceClaimTemplateSubdir, podClaimName))
	}
	return dirs
}

// A firstAvailable subrequest is reported as "<mainRequest>/<subRequest>", but
// virt-launcher globs the directory named after the main request it knows
// from the VMI.
func kubevirtMetadataRequestName(result *resourceapi.DeviceRequestAllocationResult) string {
	requestName, _, _ := strings.Cut(result.Request, "/")
	return requestName
}

func kubevirtMetadataFilePath(claimDir, requestName, driverName string) string {
	return filepath.Join(claimDir, requestName, driverName+"-metadata.json")
}

// kubevirtMetadataFiles returns the files writeKubeVirtMetadata publishes for
// requestName, one per layout; the prepare path bind-mounts exactly these.
func kubevirtMetadataFiles(basePath string, claim *resourceapi.ResourceClaim, driverName, requestName string) []string {
	var files []string
	for _, claimDir := range kubevirtMetadataClaimDirs(basePath, claim) {
		files = append(files, kubevirtMetadataFilePath(claimDir, requestName, driverName))
	}
	return files
}

func writeKubeVirtMetadata(basePath string, claim *resourceapi.ResourceClaim, driverName string, allocatable AllocatableDevices) error {
	var podClaimName *string
	if v, ok := claim.Annotations[podClaimNameAnnotation]; ok && v != "" {
		podClaimName = &v
	}

	devicesByRequest := map[string][]kubevirtMetadataDevice{}
	for i := range claim.Status.Allocation.Devices.Results {
		result := &claim.Status.Allocation.Devices.Results[i]
		if result.Driver != driverName {
			continue
		}
		device, ok := allocatable[result.Device]
		if !ok {
			continue
		}
		requestName := kubevirtMetadataRequestName(result)
		devicesByRequest[requestName] = append(devicesByRequest[requestName], kubevirtMetadataDevice{
			Driver:     driverName,
			Pool:       result.Pool,
			Name:       result.Device,
			Attributes: device.Attributes,
		})
	}

	// Ownership is claimed per layout right before writing into it. On a
	// collision (or any later error) the caller rolls back by claim UID via
	// removeKubeVirtMetadataForClaim, which finds everything written here —
	// markers included — by scanning, so nothing needs to be tracked.
	for _, claimDir := range kubevirtMetadataClaimDirs(basePath, claim) {
		if err := claimMetadataOwnership(claimDir, string(claim.UID)); err != nil {
			return err
		}
		for requestName, devices := range devicesByRequest {
			path := kubevirtMetadataFilePath(claimDir, requestName, driverName)
			dir := filepath.Dir(path)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create metadata dir %s: %w", dir, err)
			}

			meta := kubevirtDeviceMetadata{
				APIVersion:   kubevirtMetadataAPIVersion,
				Kind:         kubevirtMetadataKind,
				PodClaimName: podClaimName,
				Requests: []kubevirtDeviceMetadataRequest{
					{Name: requestName, Devices: devices},
				},
			}
			data, err := json.Marshal(meta)
			if err != nil {
				return fmt.Errorf("marshal metadata for request %s: %w", requestName, err)
			}

			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, data, 0o644); err != nil {
				return fmt.Errorf("write metadata %s: %w", tmp, err)
			}
			if err := os.Rename(tmp, path); err != nil {
				return fmt.Errorf("publish metadata %s: %w", path, err)
			}
		}
	}
	return nil
}

// claimMetadataOwnership claims the name-keyed directory for claimUID, or
// fails if another live claim already holds it. A directory without a marker
// (pre-marker driver versions, or an interrupted write) is adopted.
func claimMetadataOwnership(claimDir, claimUID string) error {
	ownerPath := filepath.Join(claimDir, kubevirtMetadataOwnerFile)
	if data, err := os.ReadFile(ownerPath); err == nil {
		if owner := strings.TrimSpace(string(data)); owner != claimUID {
			return fmt.Errorf("metadata directory %s is owned by claim %s: another claim with the same name is prepared on this node", claimDir, owner)
		}
		return nil
	}
	if err := os.MkdirAll(claimDir, 0o755); err != nil {
		return fmt.Errorf("create metadata claim dir %s: %w", claimDir, err)
	}
	if err := os.WriteFile(ownerPath, []byte(claimUID+"\n"), 0o644); err != nil {
		return fmt.Errorf("write owner marker %s: %w", ownerPath, err)
	}
	return nil
}

// removeKubeVirtMetadataForClaim removes every metadata directory owned by
// claimUID, discovering them by scanning the on-disk owner markers. Keeping
// cleanup state on disk instead of in the checkpoint keeps the checkpoint
// schema identical to pre-vfio drivers (see checkpoint.go). Directories
// whose marker names another claim are left untouched.
func removeKubeVirtMetadataForClaim(basePath, claimUID string) error {
	for _, subdir := range []string{resourceClaimsSubdir, resourceClaimTemplateSubdir} {
		parent := filepath.Join(basePath, subdir)
		entries, err := os.ReadDir(parent)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("scan metadata dir %s: %w", parent, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			claimDir := filepath.Join(parent, entry.Name())
			data, err := os.ReadFile(filepath.Join(claimDir, kubevirtMetadataOwnerFile))
			if err != nil || strings.TrimSpace(string(data)) != claimUID {
				continue
			}
			if err := os.RemoveAll(claimDir); err != nil {
				return fmt.Errorf("remove metadata dir %s: %w", claimDir, err)
			}
		}
	}
	return nil
}
