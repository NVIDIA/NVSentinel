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

package nodemetadata

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
)

func TestNewProcessor(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		platform    Platform
		config      *Config
		clientset   kubernetes.Interface
		expectError bool
		errorMsg    string
	}{
		{
			name:     "successful kubernetes processor creation",
			platform: PlatformKubernetes,
			config: &Config{
				Enabled:       true,
				CacheSize:     100,
				CacheTTL:      time.Hour,
				AllowedLabels: []string{"topology.kubernetes.io/zone"},
			},
			clientset:   testClient,
			expectError: false,
		},
		{
			name:        "unsupported platform",
			platform:    Platform("unsupported"),
			config:      &Config{Enabled: true, CacheSize: 100, CacheTTL: time.Hour},
			clientset:   testClient,
			expectError: true,
			errorMsg:    "unsupported platform",
		},
		{
			name:     "invalid config",
			platform: PlatformKubernetes,
			config: &Config{
				Enabled:   true,
				CacheSize: 0,
				CacheTTL:  time.Hour,
			},
			clientset:   testClient,
			expectError: true,
			errorMsg:    "invalid config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor, err := NewProcessor(ctx, tt.platform, tt.config, tt.clientset)

			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
				assert.Nil(t, processor)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, processor)
			}
		})
	}
}
