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

package lambda

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSetting stands in for whichever configuration key a caller names, and is
// asserted on so every error keeps pointing at the value the operator set.
const testSetting = "LAMBDA_API_ENDPOINT"

// TestNormalizeEndpoint_ValidationRules_NormalizesOrRejectsEndpoint checks the endpoint is validated before a credential
// can be sent to it, and asserts which error fires so the http rejection cannot
// be quietly folded into the general scheme check.
func TestNormalizeEndpoint_ValidationRules_NormalizesOrRejectsEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "unset defaults to production", raw: "", want: DefaultAPIEndpoint},
		{name: "trailing slash trimmed", raw: "https://cloud.lambda.ai/", want: "https://cloud.lambda.ai"},
		// Paths are appended by concatenation, so one leftover slash is enough
		// to send every request to a doubled path.
		{name: "repeated trailing slashes trimmed", raw: "https://cloud.lambda.ai///", want: "https://cloud.lambda.ai"},
		// The allowlist is fixed, so a bearer token can only ever reach a known
		// Lambda API host.
		{name: "unapproved host rejected", raw: "https://cloud.example.com", wantErr: "is not an approved"},
		{
			name: "approved host matches case-insensitively",
			raw:  "https://Cloud.Lambda.AI",
			want: "https://Cloud.Lambda.AI",
		},
		{
			name: "port is not part of the host match",
			raw:  "https://cloud.lambda.ai:8443",
			want: "https://cloud.lambda.ai:8443",
		},
		// Both credentials are bearer tokens on every request, so http would put
		// one on the wire in cleartext. There is no opt-in that relaxes this.
		{name: "plain http rejected", raw: "http://cloud.lambda.ai", wantErr: "cleartext"},
		{name: "plain http rejected for loopback too", raw: "http://127.0.0.1:8080", wantErr: "cleartext"},
		{name: "whitespace only defaults", raw: "   ", want: DefaultAPIEndpoint},
		{name: "relative URL rejected", raw: "/api/v1", wantErr: "must be an absolute https URL"},
		{name: "host only rejected", raw: "cloud.lambda.ai", wantErr: "must be an absolute https URL"},
		{name: "non-http scheme rejected", raw: "ftp://cloud.lambda.ai", wantErr: "must be an absolute https URL"},
		{name: "scheme without host rejected", raw: "https://", wantErr: "has no host"},
		// Userinfo rides along in every request URL the client logs. A query or
		// fragment is worse than cosmetic: request paths are appended by string
		// concatenation, so "?token=x" + "/api/v1/instances" leaves the path
		// inside the query.
		{name: "userinfo rejected", raw: "https://user:pass@cloud.lambda.ai", wantErr: "must not include userinfo"},
		{name: "query rejected", raw: "https://cloud.lambda.ai?token=x", wantErr: "must not include userinfo"},
		{name: "fragment rejected", raw: "https://cloud.lambda.ai#frag", wantErr: "must not include userinfo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeEndpoint(testSetting, tt.raw)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				// Every rejection names the setting, so the operator knows
				// which value to fix.
				assert.ErrorContains(t, err, testSetting)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
