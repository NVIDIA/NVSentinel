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

	"github.com/stretchr/testify/require"
)

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
		K8sConnectorConfig: K8sConnectorConfig{MaxNodeConditionMessageLength: 1024, CompactedHealthEventMsgLen: 256},
		QPS:                20,
		Burst:              40,
	}, settings)

	for key := range raw {
		partial := maps.Clone(raw)
		delete(partial, key)

		_, err := SettingsFromConfig(partial)
		require.ErrorContains(t, err, key, "every key is required")
	}
}
