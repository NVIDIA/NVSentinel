// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
)

func TestParseNVRMGPUMapLine(t *testing.T) {
	xidHandler := &XIDHandler{}

	testCases := []struct {
		name  string
		line  string
		pciId string
		gpuId string
	}{
		{
			name:  "Valid XID Error",
			line:  "NVRM: GPU at PCI:0000:00:08.0: GPU-123",
			pciId: "0000:00:08.0",
			gpuId: "GPU-123",
		},
		{
			name:  "Invalid XID Error",
			line:  "NVRM: Some other error message",
			pciId: "",
			gpuId: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pciId, gpuId := xidHandler.parseNVRMGPUMapLine(tc.line)
			assert.Equal(t, tc.pciId, pciId)
			assert.Equal(t, tc.gpuId, gpuId)
		})
	}
}

func TestNormalizePCI(t *testing.T) {
	xidHandler := &XIDHandler{}

	testCases := []struct {
		name          string
		pci           string
		normalizedPCI string
	}{
		{
			name:          "Valid PCI",
			pci:           "0000:00:08.0",
			normalizedPCI: "0000:00:08",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			normalizedPCI := xidHandler.normalizePCI(tc.pci)
			assert.Equal(t, tc.normalizedPCI, normalizedPCI)
		})
	}
}

func TestDetermineFatality(t *testing.T) {
	xidHandler, err := NewXIDHandler("test-node",
		"test-agent", "test-component", "test-check", "http://localhost:8080")
	assert.Nil(t, err)

	testCases := []struct {
		name     string
		code     pb.RecommenedAction
		fatality bool
	}{
		{
			name:     "Fatal XID",
			code:     pb.RecommenedAction_UPDATE_SWFW,
			fatality: true,
		},
		{
			name:     "Non-Fatal XID",
			code:     pb.RecommenedAction_APPLICATION_RESTART,
			fatality: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fatality := xidHandler.determineFatality(tc.code)
			assert.Equal(t, tc.fatality, fatality)
		})
	}
}
