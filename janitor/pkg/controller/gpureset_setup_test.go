// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/janitor/pkg/config"
)

func TestGPUResetReconcilerSetupWithManager_ControllerState_ValidatesRequiredConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		expectedErr string
	}{
		{
			name:    "disabled controller without job template succeeds",
			enabled: false,
		},
		{
			name:        "enabled controller without job template fails",
			enabled:     true,
			expectedErr: "failed to get valid reset job template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := &GPUResetReconciler{
				Config: &config.GPUResetControllerConfig{
					Enabled: tt.enabled,
				},
			}

			err := reconciler.SetupWithManager(nil)
			if tt.expectedErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tt.expectedErr)
		})
	}
}
