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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingT records what AllowLoopbackHostForTest does to its test, so a
// rejection can be checked without failing the real test.
type recordingT struct {
	cleanups []func()
	fatal    string
}

func (r *recordingT) Helper()           {}
func (r *recordingT) Cleanup(fn func()) { r.cleanups = append(r.cleanups, fn) }

func (r *recordingT) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}

func (r *recordingT) runCleanups() {
	for _, fn := range r.cleanups {
		fn()
	}
}

func TestNormalizeEndpoint(t *testing.T) {
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

func TestAllowLoopbackHostForTest_HostSupplied_AcceptsOnlyLoopback(t *testing.T) {
	tests := []struct {
		host   string
		accept bool
	}{
		{host: "127.0.0.1", accept: true},
		{host: "127.0.0.2", accept: true},
		{host: "::1", accept: true},
		{host: "localhost", accept: true},
		{host: "LOCALHOST", accept: true},
		{host: "api.linode.com"},
		{host: "example.com"},
		{host: "10.0.0.5"},
		{host: "0.0.0.0"},
		{host: ""},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			rec := &recordingT{}

			AllowLoopbackHostForTest(rec, tt.host)

			// Undo the call whatever happens, so a failure here cannot leak the
			// moved pin into the tests that run after this one.
			defer rec.runCleanups()

			if tt.accept {
				assert.Empty(t, rec.fatal)
				assert.Len(t, rec.cleanups, 1, "an accepted host must register its restore")

				return
			}

			assert.Contains(t, rec.fatal, "not a loopback host")
			assert.Empty(t, rec.cleanups, "a refused host must leave the pin alone")

			_, err := NormalizeEndpoint("akamai.apiEndpoint", DefaultAPIEndpoint)
			assert.NoError(t, err, "the production pin must still accept the production endpoint")
		})
	}
}

func TestNormalizeEndpoint_PinMovedToLoopback_AcceptsOnlyThatHostThenRestores(t *testing.T) {
	const local = "http://127.0.0.1:43210/v4"

	// The production pin refuses a loopback endpoint.
	_, err := NormalizeEndpoint("akamai.apiEndpoint", local)
	require.ErrorContains(t, err, "cleartext")

	t.Run("while moved", func(t *testing.T) {
		AllowLoopbackHostForTest(t, "127.0.0.1")

		got, err := NormalizeEndpoint("akamai.apiEndpoint", local)
		require.NoError(t, err)
		assert.Equal(t, local, got)

		// Only the moved host is accepted. The production host is not, and
		// neither is another loopback address.
		_, err = NormalizeEndpoint("akamai.apiEndpoint", DefaultAPIEndpoint)
		require.ErrorContains(t, err, "not an approved")

		_, err = NormalizeEndpoint("akamai.apiEndpoint", "http://127.0.0.2:43210/v4")
		require.ErrorContains(t, err, "not an approved")

		// The userinfo, query and scheme rules still apply.
		_, err = NormalizeEndpoint("akamai.apiEndpoint", "http://user:pw@127.0.0.1:43210/v4")
		require.ErrorContains(t, err, "userinfo")

		_, err = NormalizeEndpoint("akamai.apiEndpoint", "ftp://127.0.0.1:43210/v4")
		require.ErrorContains(t, err, "absolute https URL")
	})

	// The production pin is back once the test that moved it has finished.
	got, err := NormalizeEndpoint("akamai.apiEndpoint", DefaultAPIEndpoint)
	require.NoError(t, err)
	assert.Equal(t, DefaultAPIEndpoint, got)

	_, err = NormalizeEndpoint("akamai.apiEndpoint", local)
	require.ErrorContains(t, err, "cleartext")
}
