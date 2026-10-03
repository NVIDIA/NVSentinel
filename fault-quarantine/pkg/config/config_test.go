// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

func TestValidationIsPartialDrainEnabled(t *testing.T) {
	tests := []struct {
		name     string
		toml     string
		expected bool
	}{
		{name: "key absent keeps partial drain semantics", toml: "[validation]\nenabled = true\n", expected: true},
		{name: "explicit true", toml: "[validation]\nenabled = true\npartialDrainEnabled = true\n", expected: true},
		{name: "explicit false", toml: "[validation]\nenabled = true\npartialDrainEnabled = false\n", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tt.toml), 0o600))

			var cfg TomlConfig
			require.NoError(t, configmanager.LoadTOMLConfig(path, &cfg))
			assert.Equal(t, tt.expected, cfg.Validation.IsPartialDrainEnabled())
		})
	}
}
