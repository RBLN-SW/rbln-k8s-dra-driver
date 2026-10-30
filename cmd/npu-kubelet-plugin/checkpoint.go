/*
 * Copyright The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/*
Modifications Copyright 2026 Rebellions Inc.

This file is derived from github.com/kubernetes-sigs/dra-example-driver
(cmd/dra-example-kubeletplugin/checkpoint.go, tag v0.2.1) and has been
modified by Rebellions Inc. See NOTICE for details.
*/

package main

import (
	"encoding/json"

	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/checksum"
)

type Checkpoint struct {
	Checksum checksum.Checksum `json:"checksum"`
	V1       *CheckpointV1     `json:"v1,omitempty"`
}

// CheckpointV1 must keep exactly this field set: the checksum is computed
// over the marshaled struct, so adding a field breaks checksum verification
// for the version being rolled back to. KubeVirt metadata cleanup
// deliberately does NOT live here — Unprepare discovers the directories to
// remove by scanning the on-disk owner markers instead (kubevirt_metadata.go).
type CheckpointV1 struct {
	PreparedClaims PreparedClaims `json:"preparedClaims,omitempty"`
}

func newCheckpoint() *Checkpoint {
	pc := &Checkpoint{
		Checksum: 0,
		V1: &CheckpointV1{
			PreparedClaims: make(PreparedClaims),
		},
	}
	return pc
}

func (cp *Checkpoint) MarshalCheckpoint() ([]byte, error) {
	cp.Checksum = 0
	out, err := json.Marshal(*cp)
	if err != nil {
		return nil, err
	}
	cp.Checksum = checksum.New(out)
	return json.Marshal(*cp)
}

func (cp *Checkpoint) UnmarshalCheckpoint(data []byte) error {
	return json.Unmarshal(data, cp)
}

func (cp *Checkpoint) VerifyChecksum() error {
	ck := cp.Checksum
	cp.Checksum = 0
	defer func() {
		cp.Checksum = ck
	}()
	out, err := json.Marshal(*cp)
	if err != nil {
		return err
	}
	return ck.Verify(out)
}
