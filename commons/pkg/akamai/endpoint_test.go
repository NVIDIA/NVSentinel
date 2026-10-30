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

package akamai

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeEndpoint_VariousInputs_DefaultsOrRejectsUnsafe(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "empty yields the default", raw: "", want: DefaultAPIEndpoint},
		{name: "whitespace yields the default", raw: "  ", want: DefaultAPIEndpoint},
		{name: "trailing slash trimmed", raw: "https://api.linode.com/v4/", want: "https://api.linode.com/v4"},
		{name: "host compared case-insensitively", raw: "https://API.Linode.com/v4", want: "https://API.Linode.com/v4"},
		{name: "http refused", raw: "http://api.linode.com/v4", wantErr: "cleartext"},
		{name: "other scheme refused", raw: "ftp://api.linode.com/v4", wantErr: "absolute https URL"},
		{name: "foreign host refused", raw: "https://example.com/v4", wantErr: "not an approved"},
		{name: "lookalike host refused", raw: "https://api.linode.com.example.com/v4", wantErr: "not an approved"},
		{name: "userinfo refused", raw: "https://user:pw@api.linode.com/v4", wantErr: "userinfo"},
		{name: "query refused", raw: "https://api.linode.com/v4?x=1", wantErr: "query"},
		{name: "no host refused", raw: "https:///v4", wantErr: "no host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeEndpoint("akamai.apiEndpoint", tt.raw)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.ErrorContains(t, err, "akamai.apiEndpoint")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
