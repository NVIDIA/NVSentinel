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
	"fmt"
	"os"
	"path/filepath"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

type Writer struct {
	outputPath string
}

func NewWriter(outputPath string) (*Writer, error) {
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create output directory %s: %w", dir, err)
	}

	return &Writer{
		outputPath: outputPath,
	}, nil
}

func (w *Writer) Write(metadata *model.GPUMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata to JSON: %w", err)
	}

	tmpPath := w.outputPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write temporary file %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, w.outputPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename %s to %s: %w", tmpPath, w.outputPath, err)
	}

	return nil
}
