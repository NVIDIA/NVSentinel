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

package annotation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidationSessionRoundTrip(t *testing.T) {
	entries := []ValidationSessionEntry{
		{Name: "vr-1", Tests: []string{"dcgm-diag"}},
		{Name: "vr-2", Failed: true},
	}

	value, err := MarshalValidationSession(entries)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"name":"vr-1","tests":["dcgm-diag"]},{"name":"vr-2","failed":true}]`, value)

	parsed, err := ParseValidationSession(value)
	require.NoError(t, err)
	assert.Equal(t, entries, parsed)
}

func TestParseValidationSession(t *testing.T) {
	entries, err := ParseValidationSession("")
	require.NoError(t, err)
	assert.Nil(t, entries)

	_, err = ParseValidationSession("{")
	require.Error(t, err)
	assert.Contains(t, err.Error(), ValidationSessionKey)
}
