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

package grpcsink

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettingsFromConfig(t *testing.T) {
	settings, err := SettingsFromConfig(map[string]any{"GRPCSinkTarget": "sink:9000", "GRPCSinkTokenPath": "/var/run/token"})
	require.NoError(t, err)
	require.Equal(t, Settings{Target: "sink:9000", TokenPath: "/var/run/token"}, settings)

	settings, err = SettingsFromConfig(map[string]any{"GRPCSinkTarget": "sink:9000"})
	require.NoError(t, err)
	require.Empty(t, settings.TokenPath, "the token is optional")

	_, err = SettingsFromConfig(map[string]any{"GRPCSinkTarget": ""})
	require.ErrorContains(t, err, "GRPCSinkTarget")

	_, err = SettingsFromConfig(map[string]any{})
	require.ErrorContains(t, err, "GRPCSinkTarget")

	_, err = SettingsFromConfig(map[string]any{"GRPCSinkTarget": "sink:9000", "GRPCSinkTokenPath": 1})
	require.ErrorContains(t, err, "GRPCSinkTokenPath", "a present value of the wrong type is refused, not read as no token")

	_, err = SettingsFromConfig(map[string]any{"GRPCSinkTarget": true})
	require.ErrorContains(t, err, "GRPCSinkTarget")
}
