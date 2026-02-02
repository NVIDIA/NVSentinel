// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metadata

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const testMetadataJSON = `{
  "version": "1.0",
  "timestamp": "2025-01-15T10:00:00Z",
  "node_name": "test-node",
  "chassis_serial": "CHASSIS-12345",
  "gpus": [
    {
      "gpu_id": 0,
      "uuid": "GPU-00000000-0000-0000-0000-000000000000",
      "pci_address": "0000:17:00.0",
      "serial_number": "SN-GPU-0",
      "device_name": "NVIDIA A100",
      "nvlinks": [
        {
          "link_id": 0,
          "remote_pci_address": "00000008:00:00.0",
          "remote_link_id": 29
        },
        {
          "link_id": 1,
          "remote_pci_address": "00000008:00:00.0",
          "remote_link_id": 28
        }
      ]
    },
    {
      "gpu_id": 1,
      "uuid": "GPU-11111111-1111-1111-1111-111111111111",
      "pci_address": "0000:65:00.0",
      "serial_number": "SN-GPU-1",
      "device_name": "NVIDIA A100",
      "nvlinks": [
        {
          "link_id": 0,
          "remote_pci_address": "00000008:00:00.0",
          "remote_link_id": 30
        }
      ]
    }
  ],
  "nvswitches": ["00000008:00:00.0"]
}`

const testMetadataNoChassisSerial = `{
  "version": "1.0",
  "timestamp": "2025-01-15T10:00:00Z",
  "node_name": "test-node",
  "chassis_serial": null,
  "gpus": [
    {
      "gpu_id": 0,
      "uuid": "GPU-00000000-0000-0000-0000-000000000000",
      "pci_address": "0000:17:00.0",
      "serial_number": "SN-GPU-0",
      "device_name": "NVIDIA A100",
      "nvlinks": []
    }
  ],
  "nvswitches": []
}`

func TestReaderLazyLoading(t *testing.T) {
	tmpDir := t.TempDir()
	metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
	require.NoError(t, os.WriteFile(metadataFile, []byte(testMetadataJSON), 0600))

	reader := NewReader(metadataFile)
	require.Nil(t, reader.metadata)

	gpu, err := reader.GetGPUByPCI("0000:17:00.0")
	require.NoError(t, err)
	require.NotNil(t, reader.metadata)
	require.Equal(t, 0, gpu.GPUID)
	require.Equal(t, "GPU-00000000-0000-0000-0000-000000000000", gpu.UUID)
}

func TestReaderConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
	require.NoError(t, os.WriteFile(metadataFile, []byte(testMetadataJSON), 0600))

	reader := NewReader(metadataFile)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reader.GetGPUByPCI("0000:17:00.0")
			require.NoError(t, err)
		}()
	}

	wg.Wait()
}

func TestGetGPUByPCI(t *testing.T) {
	tmpDir := t.TempDir()
	metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
	require.NoError(t, os.WriteFile(metadataFile, []byte(testMetadataJSON), 0600))

	reader := NewReader(metadataFile)

	tests := []struct {
		name    string
		pci     string
		wantID  int
		wantErr bool
	}{
		{
			name:    "exact match",
			pci:     "0000:17:00.0",
			wantID:  0,
			wantErr: false,
		},
		{
			name:    "normalized match",
			pci:     "0000:65:00.0",
			wantID:  1,
			wantErr: false,
		},
		{
			name:    "long domain normalized",
			pci:     "00000000:17:00.0",
			wantID:  0,
			wantErr: false,
		},
		{
			name:    "not found",
			pci:     "0000:99:00.0",
			wantID:  -1,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gpu, err := reader.GetGPUByPCI(tt.pci)
			if tt.wantErr {
				require.Error(t, err)
				require.Nil(t, gpu)
			} else {
				require.NoError(t, err)
				require.NotNil(t, gpu)
				require.Equal(t, tt.wantID, gpu.GPUID)
			}
		})
	}
}

func TestGetInfoByUUID(t *testing.T) {
	tmpDir := t.TempDir()
	metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
	require.NoError(t, os.WriteFile(metadataFile, []byte(testMetadataJSON), 0600))

	reader := NewReader(metadataFile)

	tests := []struct {
		name    string
		uuid    string
		wantID  int
		wantErr bool
	}{
		{
			name:    "exact match for GPU 0",
			uuid:    "GPU-00000000-0000-0000-0000-000000000000",
			wantID:  0,
			wantErr: false,
		},
		{
			name:    "exact match for GPU 1",
			uuid:    "GPU-11111111-1111-1111-1111-111111111111",
			wantID:  1,
			wantErr: false,
		},
		{
			name:    "GPU not found",
			uuid:    "GPU-123",
			wantID:  -1,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gpu, err := reader.GetInfoByUUID(tt.uuid)
			if tt.wantErr {
				require.Error(t, err)
				require.Nil(t, gpu)
			} else {
				require.NoError(t, err)
				require.NotNil(t, gpu)
				require.Equal(t, tt.wantID, gpu.GPUID)
			}
		})
	}
}

func TestGetGPUByNVSwitchLink(t *testing.T) {
	tmpDir := t.TempDir()
	metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
	require.NoError(t, os.WriteFile(metadataFile, []byte(testMetadataJSON), 0600))

	reader := NewReader(metadataFile)

	tests := []struct {
		name          string
		nvswitchPCI   string
		linkID        int
		wantGPUID     int
		wantLocalLink int
		wantErr       bool
	}{
		{
			name:          "GPU 0 link 0",
			nvswitchPCI:   "00000008:00:00.0",
			linkID:        29,
			wantGPUID:     0,
			wantLocalLink: 0,
			wantErr:       false,
		},
		{
			name:          "GPU 0 link 1",
			nvswitchPCI:   "00000008:00:00.0",
			linkID:        28,
			wantGPUID:     0,
			wantLocalLink: 1,
			wantErr:       false,
		},
		{
			name:          "GPU 1 link 0",
			nvswitchPCI:   "00000008:00:00.0",
			linkID:        30,
			wantGPUID:     1,
			wantLocalLink: 0,
			wantErr:       false,
		},
		{
			name:        "nvswitch not found",
			nvswitchPCI: "0000:99:00.0",
			linkID:      0,
			wantErr:     true,
		},
		{
			name:        "link not found",
			nvswitchPCI: "00000008:00:00.0",
			linkID:      999,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gpu, localLink, err := reader.GetGPUByNVSwitchLink(tt.nvswitchPCI, tt.linkID)
			if tt.wantErr {
				require.Error(t, err)
				require.Nil(t, gpu)
			} else {
				require.NoError(t, err)
				require.NotNil(t, gpu)
				require.Equal(t, tt.wantGPUID, gpu.GPUID)
				require.Equal(t, tt.wantLocalLink, localLink)
			}
		})
	}
}

func TestGetChassisSerial(t *testing.T) {
	t.Run("with chassis serial", func(t *testing.T) {
		tmpDir := t.TempDir()
		metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
		require.NoError(t, os.WriteFile(metadataFile, []byte(testMetadataJSON), 0600))

		reader := NewReader(metadataFile)
		serial := reader.GetChassisSerial()
		require.NotNil(t, serial)
		require.Equal(t, "CHASSIS-12345", *serial)
	})

	t.Run("without chassis serial", func(t *testing.T) {
		tmpDir := t.TempDir()
		metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
		require.NoError(t, os.WriteFile(metadataFile, []byte(testMetadataNoChassisSerial), 0600))

		reader := NewReader(metadataFile)
		serial := reader.GetChassisSerial()
		require.Nil(t, serial)
	})
}

func TestReaderFileNotFound(t *testing.T) {
	reader := NewReader("/nonexistent/file.json")
	_, err := reader.GetGPUByPCI("0000:17:00.0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to load metadata for PCI lookup")
	require.Contains(t, err.Error(), "failed to read metadata file")
}

func TestReaderMalformedJSON(t *testing.T) {
	tmpDir := t.TempDir()
	metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
	require.NoError(t, os.WriteFile(metadataFile, []byte("{invalid json"), 0600))

	reader := NewReader(metadataFile)
	_, err := reader.GetGPUByPCI("0000:17:00.0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to load metadata for PCI lookup")
	require.Contains(t, err.Error(), "failed to parse metadata JSON")
}

func TestReaderErrorCaching(t *testing.T) {
	reader := NewReader("/nonexistent/file.json")

	_, err1 := reader.GetGPUByPCI("0000:17:00.0")
	_, err2 := reader.GetGPUByPCI("0000:65:00.0")

	require.Error(t, err1)
	require.Error(t, err2)
	require.Contains(t, err1.Error(), "failed to load metadata for PCI lookup 0000:17:00.0")
	require.Contains(t, err2.Error(), "failed to load metadata for PCI lookup 0000:65:00.0")
	require.Contains(t, err1.Error(), "failed to read metadata file")
	require.Contains(t, err2.Error(), "failed to read metadata file")
}

func TestGetGPUByNVSwitchLinkErrorWrapping(t *testing.T) {
	reader := NewReader("/nonexistent/file.json")
	_, _, err := reader.GetGPUByNVSwitchLink("0008:00:00.0", 29)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to load metadata for NVSwitch lookup")
	require.Contains(t, err.Error(), "0008:00:00.0")
	require.Contains(t, err.Error(), "link 29")
	require.Contains(t, err.Error(), "failed to read metadata file")
}

func TestNormalizePCI(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"0000:17:00.0", "0000:17:00"},
		{"00000000:17:00.0", "0000:17:00"},
		{"00000008:00:00.0", "0008:00:00"},
		{"FFFF:AB:CD.0", "ffff:ab:cd"},
		{"0000:17:00", "0000:17:00"},
		{"invalid-pci", "invalid-pci"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := normalizePCI(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}
