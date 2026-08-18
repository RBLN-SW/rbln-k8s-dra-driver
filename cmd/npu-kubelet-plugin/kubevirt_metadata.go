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
// The directory is not mounted into virt-launcher by KubeVirt either; the
// prepare path injects a read-only CDI mount of the base directory alongside
// the vfio device nodes.
const (
	kubevirtMetadataBasePath    = "/var/run/kubernetes.io/dra-device-attributes"
	kubevirtMetadataAPIVersion  = "metadata.resource.k8s.io/v1alpha1"
	kubevirtMetadataKind        = "DeviceMetadata"
	resourceClaimsSubdir        = "resourceclaims"
	resourceClaimTemplateSubdir = "resourceclaimtemplates"
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
	Driver     string                                                  `json:"driver"`
	Pool       string                                                  `json:"pool"`
	Name       string                                                  `json:"name"`
	Attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute `json:"attributes,omitempty"`
}

func kubevirtMetadataClaimDir(basePath string, claim *resourceapi.ResourceClaim) string {
	if podClaimName, ok := claim.Annotations[podClaimNameAnnotation]; ok && podClaimName != "" {
		return filepath.Join(basePath, resourceClaimTemplateSubdir, podClaimName)
	}
	return filepath.Join(basePath, resourceClaimsSubdir, claim.Name)
}

func writeKubeVirtMetadata(basePath string, claim *resourceapi.ResourceClaim, driverName string, allocatable AllocatableDevices) ([]string, error) {
	claimDir := kubevirtMetadataClaimDir(basePath, claim)

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
		devicesByRequest[result.Request] = append(devicesByRequest[result.Request], kubevirtMetadataDevice{
			Driver:     driverName,
			Pool:       result.Pool,
			Name:       result.Device,
			Attributes: device.Attributes,
		})
	}

	var written []string
	for requestName, devices := range devicesByRequest {
		dir := filepath.Join(claimDir, requestName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return written, fmt.Errorf("create metadata dir %s: %w", dir, err)
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
			return written, fmt.Errorf("marshal metadata for request %s: %w", requestName, err)
		}

		path := filepath.Join(dir, driverName+"-metadata.json")
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return written, fmt.Errorf("write metadata %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return written, fmt.Errorf("publish metadata %s: %w", path, err)
		}
		written = append(written, dir)
	}
	return written, nil
}

func removeKubeVirtMetadata(dirs []string) error {
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove metadata dir %s: %w", dir, err)
		}
		// Best-effort prune of the per-claim parent once it is empty.
		_ = os.Remove(filepath.Dir(dir))
	}
	return nil
}
