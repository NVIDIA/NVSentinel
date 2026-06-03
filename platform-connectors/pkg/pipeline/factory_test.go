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
