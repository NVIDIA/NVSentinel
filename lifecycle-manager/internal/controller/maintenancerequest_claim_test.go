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

package controller

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestClaimLeaseName_FitsAndStaysReadable(t *testing.T) {
	t.Parallel()

	// "mr-claim." plus the kept prefix fills 244 characters before "-<8 hex>",
	// so node[234] is the last character kept from a node name that is too long.
	const lastKept = 234

	hashSuffix := regexp.MustCompile(`-[0-9a-f]{8}$`)

	tests := []struct {
		name       string
		nodeName   string
		want       string
		wantHashed bool
	}{
		{
			name:     "typical node name is prefixed unchanged",
			nodeName: "gpu-node-7",
			want:     "mr-claim.gpu-node-7",
		},
		{
			name:     "name that exactly fits 253 characters is unchanged",
			nodeName: strings.Repeat("a", validation.DNS1123SubdomainMaxLength-len(claimLeasePrefix)),
			want:     claimLeasePrefix + strings.Repeat("a", validation.DNS1123SubdomainMaxLength-len(claimLeasePrefix)),
		},
		{
			name:       "name one character too long keeps its start and gains a hash",
			nodeName:   strings.Repeat("a", validation.DNS1123SubdomainMaxLength-len(claimLeasePrefix)+1),
			wantHashed: true,
		},
		{
			name:       "cut landing on a dot drops the dot",
			nodeName:   strings.Repeat("a", lastKept) + "." + strings.Repeat("b", 20),
			wantHashed: true,
		},
		{
			name:       "cut landing on a dash drops the dash",
			nodeName:   strings.Repeat("a", lastKept) + "-" + strings.Repeat("b", 20),
			wantHashed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ClaimLeaseName(tt.nodeName)

			assert.Empty(t, validation.IsDNS1123Subdomain(got), "claim name %q must be a valid lease name", got)
			assert.NotEqual(t, tt.nodeName, got, "claim must not collide with the janitor lock for the same node")

			if !tt.wantHashed {
				assert.Equal(t, tt.want, got)

				return
			}

			assert.LessOrEqual(t, len(got), validation.DNS1123SubdomainMaxLength)
			assert.Regexp(t, hashSuffix, got)
			assert.True(t, strings.HasPrefix(got, claimLeasePrefix+strings.Repeat("a", lastKept)),
				"the readable start of the node name must be kept")
			assert.NotContains(t, got, ".-")
			assert.NotContains(t, got, "--")
		})
	}
}

func TestClaimLeaseName_LongNamesWithSharedStartDoNotCollide(t *testing.T) {
	t.Parallel()

	shared := strings.Repeat("a", 250)

	first := ClaimLeaseName(shared + "-1")
	second := ClaimLeaseName(shared + "-2")

	require.NotEqual(t, first, second)
}
