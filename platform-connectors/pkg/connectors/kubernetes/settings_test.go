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

package kubernetes

import (
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
)

// settingsFromJSON decodes config.json text the way configfile.Load does
// (numbers as json.Number), so these tests see the types a rendered ConfigMap
// produces.
func settingsFromJSON(t *testing.T, raw string) map[string]any {
	t.Helper()

	m, err := configfile.Decode([]byte(raw))
	require.NoError(t, err)

	return m
}

func TestSettingsFromConfig(t *testing.T) {
	raw := map[string]any{
		"K8sConnectorQps":               json.Number("20.00"),
		"K8sConnectorBurst":             json.Number("40"),
		"MaxNodeConditionMessageLength": json.Number("1024"),
		"CompactedHealthEventMsgLen":    json.Number("256"),
	}

	settings, err := SettingsFromConfig(raw)
	require.NoError(t, err)
	require.Equal(t, Settings{
		K8sConnectorConfig: K8sConnectorConfig{
			MaxNodeConditionMessageLength: 1024,
			CompactedHealthEventMsgLen:    256,
		},
		QPS:   20,
		Burst: 40,
	}, settings)

	for key := range raw {
		partial := maps.Clone(raw)
		delete(partial, key)

		_, err := SettingsFromConfig(partial)
		require.ErrorContains(t, err, key, "every key is required")
	}
}

// TestSettingsFromConfig_ReadsTheRetrySettings: the two optional retry keys
// reach the connector configuration through the shared reader.
func TestSettingsFromConfig_ReadsTheRetrySettings(t *testing.T) {
	settings, err := SettingsFromConfig(settingsFromJSON(t, `{
		"K8sConnectorQps": 20.00, "K8sConnectorBurst": 40,
		"MaxNodeConditionMessageLength": 1024, "CompactedHealthEventMsgLen": 256,
		"K8sConnectorMaxRetries": 7, "K8sConnectorMaxRetryDuration": "45s"
	}`))
	require.NoError(t, err)
	require.Equal(t, 7, settings.MaxRetries)
	require.Equal(t, 45*time.Second, settings.MaxRetryDuration)

	_, err = SettingsFromConfig(settingsFromJSON(t, `{
		"K8sConnectorQps": 20.00, "K8sConnectorBurst": 40,
		"MaxNodeConditionMessageLength": 1024, "CompactedHealthEventMsgLen": 256,
		"K8sConnectorMaxRetries": 1.5
	}`))
	require.ErrorContains(t, err, "K8sConnectorMaxRetries")
}
