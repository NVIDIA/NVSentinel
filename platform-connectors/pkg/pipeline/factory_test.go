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

package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewFromConfigs_PassesOptionsToFactory(t *testing.T) {
	const transformerName = "TestOptionsTransformer"

	var receivedOpts Options
	Register(transformerName, func(cfg *Config, opts Options) (Transformer, error) {
		receivedOpts = opts
		return &mockTransformer{name: cfg.Name}, nil
	})
	t.Cleanup(func() {
		delete(registry, transformerName)
	})

	_, err := NewFromConfigs(context.Background(), []Config{
		{
			Name:       transformerName,
			Enabled:    true,
			ConfigPath: "/tmp/metadata.toml",
		},
	}, Options{
		KubeconfigPath: "/var/lib/kubelet/kubeconfig",
	})
	require.NoError(t, err)
	require.Equal(t, "/var/lib/kubelet/kubeconfig", receivedOpts.KubeconfigPath)
}

// TestNewFromRawConfig_ErrorContract pins the error each malformed raw config
// shape produces: both roles parse config.json through NewFromRawConfig, so
// these messages are what an operator sees on a bad deploy.
func TestNewFromRawConfig_ErrorContract(t *testing.T) {
	tests := []struct {
		name    string
		rawCfg  map[string]any
		wantErr string
	}{
		{
			name:    "missing pipeline key",
			rawCfg:  map[string]any{"port": 5001},
			wantErr: "no pipeline configuration found",
		},
		{
			name:    "empty pipeline list",
			rawCfg:  map[string]any{"pipeline": []any{}},
			wantErr: "no pipeline configuration found",
		},
		{
			name:    "pipeline is not a list",
			rawCfg:  map[string]any{"pipeline": "MetadataAugmentor"},
			wantErr: "no pipeline configuration found",
		},
		{
			name:    "non-map item",
			rawCfg:  map[string]any{"pipeline": []any{"not-a-map"}},
			wantErr: "failed to convert pipeline configuration to map",
		},
		{
			name: "missing name field",
			rawCfg: map[string]any{"pipeline": []any{map[string]any{
				"enabled": true, "config": "/etc/metadata.toml",
			}}},
			wantErr: "missing or invalid 'name' field",
		},
		{
			name: "mistyped name field",
			rawCfg: map[string]any{"pipeline": []any{map[string]any{
				"name": 42, "enabled": true, "config": "/etc/metadata.toml",
			}}},
			wantErr: "missing or invalid 'name' field",
		},
		{
			name: "missing enabled field",
			rawCfg: map[string]any{"pipeline": []any{map[string]any{
				"name": "MetadataAugmentor", "config": "/etc/metadata.toml",
			}}},
			wantErr: "missing or invalid 'enabled' field",
		},
		{
			name: "mistyped enabled field",
			rawCfg: map[string]any{"pipeline": []any{map[string]any{
				"name": "MetadataAugmentor", "enabled": "true", "config": "/etc/metadata.toml",
			}}},
			wantErr: "missing or invalid 'enabled' field",
		},
		{
			name: "missing config field",
			rawCfg: map[string]any{"pipeline": []any{map[string]any{
				"name": "MetadataAugmentor", "enabled": true,
			}}},
			wantErr: "missing or invalid 'config' field",
		},
		{
			name: "mistyped config field",
			rawCfg: map[string]any{"pipeline": []any{map[string]any{
				"name": "MetadataAugmentor", "enabled": true, "config": 7,
			}}},
			wantErr: "missing or invalid 'config' field",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewFromRawConfig(context.Background(), tt.rawCfg, Options{})
			require.Error(t, err)
			require.ErrorContains(t, err, tt.wantErr)
			require.Nil(t, p)
		})
	}
}

// TestNewFromRawConfig_HappyPath verifies a well-formed raw config reaches a
// registered factory and yields a pipeline.
func TestNewFromRawConfig_HappyPath(t *testing.T) {
	const transformerName = "TestRawConfigTransformer"

	var receivedCfg Config

	Register(transformerName, func(cfg *Config, _ Options) (Transformer, error) {
		receivedCfg = *cfg
		return &mockTransformer{name: cfg.Name}, nil
	})
	t.Cleanup(func() {
		delete(registry, transformerName)
	})

	p, err := NewFromRawConfig(context.Background(), map[string]any{
		"pipeline": []any{map[string]any{
			"name":    transformerName,
			"enabled": true,
			"config":  "/etc/metadata.toml",
		}},
	}, Options{})
	require.NoError(t, err)
	require.NotNil(t, p)
	require.Equal(t, transformerName, receivedCfg.Name)
	require.True(t, receivedCfg.Enabled)
	require.Equal(t, "/etc/metadata.toml", receivedCfg.ConfigPath)
}
