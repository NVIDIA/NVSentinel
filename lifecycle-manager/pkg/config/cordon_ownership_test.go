// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

	"github.com/stretchr/testify/require"
)

func TestLoadRawCordonOnlyOwned_DefaultAndExplicit(t *testing.T) {
	for _, tc := range []struct {
		value    string
		expected bool
	}{
		{"", false}, {"onlyOwned: false", false}, {"onlyOwned: true", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := "apiVersion: nvsentinel.nvidia.com/v1alpha1\nkind: ValidationConfiguration\n" +
				"spec:\n  schedulingGate:\n    cordon:\n      remove: true\n      " + tc.value + "\n"
			require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
			config, err := loadRaw(path)
			require.NoError(t, err)
			require.Equal(t, tc.expected, config.Spec.SchedulingGate.Cordon.OnlyOwned)
			require.Equal(t, tc.expected, config.DeepCopy().Spec.SchedulingGate.Cordon.OnlyOwned)
		})
	}
}
