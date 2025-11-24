// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package parser

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSidecarParser_Parse(t *testing.T) {
	testCases := []struct {
		name              string
		message           string
		mockResponse      *Response
		mockStatusCode    int
		expectedSuccess   bool
		expectedXIDCode   int
		expectedPCIAddr   string
		expectedMnemonic  string
		expectedErrorCode string
		expectError       bool
		expectedErrorType string
	}{
		{
			name:    "Successful XID parsing with full details",
			message: "NVRM: Xid (PCI:0000:66:00): 32, pid=2280636, name=train.3, Channel ID 0000000d intr0 00040000",
			mockResponse: &Response{
				Success: true,
				Result: XIDDetails{
					Context:             "GPU context",
					DecodedXIDStr:       "32",
					Driver:              "nvidia-driver-550",
					InvestigatoryAction: "Check GPU memory",
					Machine:             "DGX-A100",
					Mnemonic:            "XID 32",
					Name:                "32",
					Number:              32,
					PCIE:                "0000:66:00",
					Resolution:          "APPLICATION_RESTART",
				},
			},
			mockStatusCode:    200,
			expectedSuccess:   true,
			expectedXIDCode:   32,
			expectedPCIAddr:   "0000:66:00",
			expectedMnemonic:  "XID 32",
			expectedErrorCode: "32",
		},
		{
			name:    "Sidecar parsing failure",
			message: "NVRM: Invalid message format",
			mockResponse: &Response{
				Success: false,
				Error:   "Failed to parse XID message",
			},
			mockStatusCode:  200,
			expectedSuccess: false,
		},
		{
			name:              "HTTP server error",
			message:           "NVRM: Xid (PCI:0000:66:00): 32, pid=1234",
			mockStatusCode:    500,
			expectError:       true,
			expectedErrorType: "request_sending_error",
		},
		{
			name:    "Minimal response fields",
			message: "NVRM: Xid (PCI:0000:9b:00): 46, pid=1234",
			mockResponse: &Response{
				Success: true,
				Result: XIDDetails{
					Number: 46,
					PCIE:   "0000:9b:00",
				},
			},
			mockStatusCode:    200,
			expectedSuccess:   true,
			expectedXIDCode:   46,
			expectedPCIAddr:   "0000:9b:00",
			expectedMnemonic:  "",
			expectedErrorCode: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var capturedRequest *Request
			var capturedMethod string
			var capturedContentType string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedMethod = r.Method
				capturedContentType = r.Header.Get("Content-Type")

				var req Request
				if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
					capturedRequest = &req
				}

				w.WriteHeader(tc.mockStatusCode)

				if tc.mockStatusCode == 200 && tc.mockResponse != nil {
					w.Header().Set("Content-Type", "application/json")
					if err := json.NewEncoder(w).Encode(tc.mockResponse); err != nil {
						t.Fatalf("encode response: %v", err)
					}
				} else if tc.mockStatusCode != 200 {
					if _, err := w.Write([]byte("Server error")); err != nil {
						t.Fatalf("write error response: %v", err)
					}
				}
			}))
			defer server.Close()

			parser := NewSidecarParser(server.URL, "test-node")

			result, err := parser.Parse(tc.message)

			if !tc.expectError || tc.mockStatusCode == 200 {
				assert.Equal(t, "POST", capturedMethod, "Should use POST method")
				assert.Equal(t, "application/json", capturedContentType, "Should set JSON content type")
				if capturedRequest != nil {
					assert.Equal(t, tc.message, capturedRequest.XIDMessage, "Request should contain the XID message")
				}
			}

			if tc.expectError {
				require.Error(t, err, "Expected error for test case")
				return
			}

			if !tc.expectedSuccess {
				require.NotNil(t, result, "Result should not be nil when no error")
				assert.False(t, result.Success, "Expected Success to be false for unsuccessful case")
				return
			}

			require.NoError(t, err, "Parse should not return error for valid response")
			require.NotNil(t, result, "Result should not be nil for valid response")
			assert.True(t, result.Success, "Parse should succeed for valid response")

			assert.Equal(t, tc.expectedXIDCode, result.Result.Number, "XID code should match")
			assert.Equal(t, tc.expectedPCIAddr, result.Result.PCIE, "PCI address should match")
			assert.Equal(t, tc.expectedMnemonic, result.Result.Mnemonic, "Mnemonic should match")
			assert.Equal(t, tc.expectedErrorCode, result.Result.DecodedXIDStr, "Decoded XID string should match")
			assert.Equal(t, tc.expectedErrorCode, result.Result.Name, "Name should match")

			assert.Empty(t, result.Error, "Error field should be empty for successful parse")
		})
	}
}
