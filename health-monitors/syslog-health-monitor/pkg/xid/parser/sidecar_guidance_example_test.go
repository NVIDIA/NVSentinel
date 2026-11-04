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

package parser

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSidecarParser_Parse_XID149ExamplePreservesGuidance(t *testing.T) {
	const message = "NVRM: Xid (PCI:0008:06:00): 149, NETIR_LINK_DOWN  Fatal   XC0 i0 Link 03 " +
		"(0x02540c91 0x00000000 0x00000000 0x00000000 0x00000000 0x00000000)"
	const driverVersion = "580.173.02"

	// The input uses an observed XID format. The analyzer response is synthetic:
	// this verifies transport and field preservation, not a catalog decision.
	const response = `{"success":true,"result":{` +
		`"number":149,"decoded_xid_string":"149","pcie_bdf":"0008:06:00",` +
		`"resolution":"NONE","investigatory_action":"Example investigation",` +
		`"comments":"Example decode comment"}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/decode-xid", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		assert.Equal(t, message, request.XIDMessage)
		assert.Equal(t, driverVersion, request.DriverVersion)
		w.Header().Set("Content-Type", "application/json")

		if _, err := io.WriteString(w, response); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	p := NewSidecarParser(server.URL, "test-node", func() string { return driverVersion })
	result, err := p.Parse(message)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Success)
	assert.Equal(t, 149, result.Result.Number)
	assert.Equal(t, "149", result.Result.DecodedXIDStr)
	assert.Equal(t, "0008:06:00", result.Result.PCIE)
	assert.Equal(t, "NONE", result.Result.Resolution)
	assert.Equal(t, "Example investigation", result.Result.InvestigatoryAction)
	assert.Equal(t, "Example decode comment", result.Result.Comments)
}
