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

package configfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

func TestLoad_ReadsTogglesAndNumbersAsTheChartWritesThem(t *testing.T) {
	raw, err := Load(writeConfig(t, `{
		"enableK8sPlatformConnector": "true",
		"K8sConnectorQps": 20.00,
		"K8sConnectorBurst": 40,
		"GRPCSinkTarget": "",
		"pipeline": []
	}`))
	require.NoError(t, err)

	require.True(t, Bool(raw, "enableK8sPlatformConnector"))

	qps, err := Float64(raw, "K8sConnectorQps")
	require.NoError(t, err)
	require.Equal(t, float64(20), qps)

	burst, err := Int64(raw, "K8sConnectorBurst")
	require.NoError(t, err)
	require.Equal(t, int64(40), burst)

	burstAsFloat, err := Float64(raw, "K8sConnectorBurst")
	require.NoError(t, err)
	require.Equal(t, float64(40), burstAsFloat)

	_, err = Int64(raw, "K8sConnectorQps")
	require.ErrorContains(t, err, "K8sConnectorQps", "a decimal is not a whole number")

	_, err = Int64(raw, "pipeline")
	require.ErrorContains(t, err, "not a number")

	_, err = Float64(raw, "missing")
	require.ErrorContains(t, err, `"missing" missing`)
}

func TestLoad_Errors(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	require.ErrorContains(t, err, "failed to read config")

	_, err = Load(writeConfig(t, `{"enableK8sPlatformConnector": `))
	require.ErrorContains(t, err, "failed to unmarshal config")

	_, err = Load(writeConfig(t, `{"a": 1} {"b": 2}`))
	require.ErrorContains(t, err, "more than one JSON value")

	_, err = Load(writeConfig(t, `{"a": 1} trailing`))
	require.ErrorContains(t, err, "content after the JSON object")
}

func TestString(t *testing.T) {
	m := map[string]any{"target": "sink:9000", "empty": "", "null": nil, "number": json.Number("1")}

	for key, want := range map[string]string{"target": "sink:9000", "empty": "", "null": "", "missing": ""} {
		got, err := String(m, key)
		require.NoError(t, err, key)
		require.Equal(t, want, got, key)
	}

	_, err := String(m, "number")
	require.ErrorContains(t, err, `"number" must be a string`)
}

func TestBool(t *testing.T) {
	m := map[string]any{"quoted": "true", "raw": true, "off": "false", "number": json.Number("1")}
	require.True(t, Bool(m, "quoted"))
	require.True(t, Bool(m, "raw"))
	require.False(t, Bool(m, "off"))
	require.False(t, Bool(m, "number"))
	require.False(t, Bool(m, "missing"))
}
