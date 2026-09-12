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

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	k8sconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
)

// TestK8sConnectorMaxRetriesFromConfig_JSONValues_DefaultsOrRejects verifies the
// actual JSON number types accepted from a rendered platform-connector ConfigMap.
func TestK8sConnectorMaxRetriesFromConfig_JSONValues_DefaultsOrRejects(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{name: "omitted", raw: `{}`, want: k8sconnector.DefaultMaxRetries},
		{name: "zero selects default", raw: `{"K8sConnectorMaxRetries":0}`, want: k8sconnector.DefaultMaxRetries},
		{name: "positive override", raw: `{"K8sConnectorMaxRetries":20}`, want: 20},
		{name: "negative", raw: `{"K8sConnectorMaxRetries":-1}`, wantErr: true},
		{name: "fraction", raw: `{"K8sConnectorMaxRetries":1.5}`, wantErr: true},
		{name: "string", raw: `{"K8sConnectorMaxRetries":"3"}`, wantErr: true},
		{name: "boolean", raw: `{"K8sConnectorMaxRetries":false}`, wantErr: true},
		{name: "null", raw: `{"K8sConnectorMaxRetries":null}`, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := k8sConnectorMaxRetriesFromConfig(configFromJSON(t, test.raw))
			if test.wantErr {
				require.ErrorContains(t, err, "must be a non-negative integer")
			} else {
				require.NoError(t, err)
				require.Equal(t, test.want, value)
			}
		})
	}
}
