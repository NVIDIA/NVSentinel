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

package xid

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/parser"
)

func TestProcessLine_SidecarGuidance_PreservesRecoveryBehavior(t *testing.T) {
	testCases := []struct {
		name       string
		message    string
		fields     string
		wantSuffix string
	}{
		{
			name:       "action supplied",
			fields:     `,"investigatory_action":"Check device diagnostics"`,
			wantSuffix: ", Investigatory Action=Check device diagnostics",
		},
		{
			name: "missing action",
		},
		{
			name:   "null action",
			fields: `,"investigatory_action":null`,
		},
		{
			name:   "empty action",
			fields: `,"investigatory_action":""`,
		},
		{
			name:   "blank action",
			fields: `,"investigatory_action":" \t\n "`,
		},
		{
			name:       "padded action",
			fields:     `,"investigatory_action":" \nCheck device diagnostics\t "`,
			wantSuffix: ", Investigatory Action=Check device diagnostics",
		},
		{
			name:       "original message preserved",
			message:    "Synthetic XID message\n",
			fields:     `,"investigatory_action":"Check device diagnostics"`,
			wantSuffix: ", Investigatory Action=Check device diagnostics",
		},
	}

	recoveries := []struct {
		name       string
		xid        int
		resolution string
		wantAction pb.RecommendedAction
		wantFatal  bool
		wantEvents int
	}{
		{
			name:       "nonfatal XID",
			xid:        149,
			resolution: "XID_154_EVAL",
			wantAction: pb.RecommendedAction_NONE,
			wantEvents: 1,
		},
		{
			name:       "XID 154 requests reset",
			xid:        154,
			resolution: "COMPONENT_RESET",
			wantAction: pb.RecommendedAction_COMPONENT_RESET,
			wantFatal:  true,
			wantEvents: 1,
		},
		{
			name:       "XID 154 returns to None",
			xid:        154,
			resolution: "NONE",
			wantAction: pb.RecommendedAction_NONE,
			wantEvents: 2,
		},
	}

	for _, tc := range testCases {
		for _, recovery := range recoveries {
			t.Run(tc.name+"/"+recovery.name, func(t *testing.T) {
				response := fmt.Sprintf(
					`{"success":true,"result":{"number":%d,`+
						`"decoded_xid_string":"%d","pcie_bdf":"0000:00:08.0",`+
						`"resolution":%q%s}}`,
					recovery.xid, recovery.xid, recovery.resolution, tc.fields,
				)

				server := httptest.NewServer(http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")

						if _, err := io.WriteString(w, response); err != nil {
							t.Errorf("write sidecar response: %v", err)
						}
					},
				))
				defer server.Close()

				handler := newHandlerWithMetadata(t)
				handler.parser = parser.NewSidecarParser(
					server.URL, "test-node", func() string { return "580.173.02" },
				)

				original := tc.message
				if original == "" {
					original = "Synthetic XID message"
				}

				events, err := handler.ProcessLine(original)
				require.NoError(t, err)
				require.NotNil(t, events)
				require.Len(t, events.Events, recovery.wantEvents)

				event := events.Events[0]
				assert.Equal(t, original+tc.wantSuffix, event.Message)
				assert.Equal(t, recovery.wantAction, event.RecommendedAction)
				assert.Equal(t, recovery.wantFatal, event.IsFatal)
				assert.False(t, event.IsHealthy)
				assert.Equal(t, []string{fmt.Sprint(recovery.xid)}, event.ErrorCode)
				assert.Equal(t, "test-node", event.NodeName)
				assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, event.ProcessingStrategy)
				assert.Empty(t, event.Metadata)
				assert.Contains(t, event.EntitiesImpacted, &pb.Entity{
					EntityType: "GPU_UUID", EntityValue: "GPU-123",
				})

				if recovery.wantEvents == 2 {
					clear := events.Events[1]
					assert.True(t, clear.IsHealthy)
					assert.False(t, clear.IsFatal)
					assert.Equal(t, pb.RecommendedAction_NONE, clear.RecommendedAction)
					assert.Equal(t, "GPU recovery action returned to None", clear.Message)
					assert.Empty(t, clear.ErrorCode)
					assert.Equal(t, event.EntitiesImpacted, clear.EntitiesImpacted)
				}
			})
		}
	}
}
