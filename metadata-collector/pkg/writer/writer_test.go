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

package writer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

func TestWriterAtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "metadata.json")

	w, err := NewWriter(outputPath)
	require.NoError(t, err, "Failed to create writer")

	metadata := &model.GPUMetadata{
		Version:    "1.0",
		Timestamp:  "2025-11-05T12:00:00Z",
		NodeName:   "test-node",
		GPUs:       []model.GPUInfo{},
		NVSwitches: []string{},
	}

	err = w.Write(metadata)
	require.NoError(t, err, "Failed to write metadata")

	_, err = os.Stat(outputPath)
	require.NoError(t, err, "Output file was not created")

	tmpPath := outputPath + ".tmp"
	_, err = os.Stat(tmpPath)
	require.True(t, os.IsNotExist(err), "Temporary file was not cleaned up")

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err, "Failed to read output file")

	var readMetadata model.GPUMetadata
	err = json.Unmarshal(data, &readMetadata)
	require.NoError(t, err, "Failed to unmarshal metadata")

	require.Equal(t, metadata.Version, readMetadata.Version, "Version mismatch")
}

func TestWriterCreateDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "subdir", "metadata.json")

	w, err := NewWriter(outputPath)
	require.NoError(t, err, "Failed to create writer")

	metadata := &model.GPUMetadata{
		Version:    "1.0",
		Timestamp:  "2025-11-05T12:00:00Z",
		NodeName:   "test-node",
		GPUs:       []model.GPUInfo{},
		NVSwitches: []string{},
	}

	err = w.Write(metadata)
	require.NoError(t, err, "Failed to write metadata")

	_, err = os.Stat(filepath.Join(tmpDir, "subdir"))
	require.NoError(t, err, "Output directory was not created")
}
