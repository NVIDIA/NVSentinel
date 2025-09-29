// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package common

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
)

func TestActionMapping(t *testing.T) {
	sxidFilePath, actionMappingPath, err := moveXIDAndActionMappingToTempDir()
	assert.Nil(t, err)
	defer func() {
		if err := os.Remove(sxidFilePath); err != nil {
			t.Logf("Failed to remove SXID file %s: %v", sxidFilePath, err)
		}
		if err := os.Remove(actionMappingPath); err != nil {
			t.Logf("Failed to remove action mapping file %s: %v", actionMappingPath, err)
		}
	}()
	err = LoadActionMappings(actionMappingPath)
	assert.Nil(t, err)

	testCases := []struct {
		name          string
		actionStr     string
		expectedCode  int
		expectMapping bool
	}{
		{
			name:          "Valid Action",
			actionStr:     "NODE_REBOOT",
			expectedCode:  int(pb.RecommenedAction_NODE_REBOOT),
			expectMapping: true,
		},
		{
			name:          "Unknown Action",
			actionStr:     "UNKNOWN_ACTION",
			expectedCode:  int(pb.RecommenedAction_REPORT_ISSUE),
			expectMapping: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			action, ok := MapActionStringToProto(tc.actionStr)
			assert.Equal(t, tc.expectMapping, ok)
			assert.Equal(t, pb.RecommenedAction(tc.expectedCode), action)
		})
	}
}

func TestLoadErrorResolutionMap(t *testing.T) {
	sxiderrormappingpath, actionMappingPath, err := moveXIDAndActionMappingToTempDir()
	defer func() {
		if err := os.Remove(sxiderrormappingpath); err != nil {
			t.Logf("Failed to remove SXID error mapping file %s: %v", sxiderrormappingpath, err)
		}
		if err := os.Remove(actionMappingPath); err != nil {
			t.Logf("Failed to remove action mapping file %s: %v", actionMappingPath, err)
		}
	}()
	assert.Nil(t, err)

	err = LoadActionMappings(actionMappingPath)
	assert.Nil(t, err)

	sxidMap, err := LoadErrorResolutionMap(sxiderrormappingpath)
	assert.Nil(t, err)

	assert.Equal(t, types.ErrorResolution{
		RecommendedAction: pb.RecommenedAction_RESET_GPU,
	}, sxidMap[24007])
}

func TestMapActionStringToProto(t *testing.T) {
	sxidFilePath, actionMappingPath, err := moveXIDAndActionMappingToTempDir()
	assert.Nil(t, err)
	defer func() {
		if err := os.Remove(sxidFilePath); err != nil {
			t.Logf("Failed to remove SXID file %s: %v", sxidFilePath, err)
		}
		if err := os.Remove(actionMappingPath); err != nil {
			t.Logf("Failed to remove action mapping file %s: %v", actionMappingPath, err)
		}
	}()
	assert.Nil(t, err)

	err = LoadActionMappings(actionMappingPath)
	assert.Nil(t, err)

	testcases := []struct {
		input          string
		expectedOutput pb.RecommenedAction
	}{
		{
			input:          "RESET_GPU",
			expectedOutput: pb.RecommenedAction_RESET_GPU,
		},
		{
			input:          "NODE_REBOOT",
			expectedOutput: pb.RecommenedAction_NODE_REBOOT,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.input, func(t *testing.T) {
			output, _ := MapActionStringToProto(tc.input)
			assert.Equal(t, tc.expectedOutput, output)
		})
	}
}

// moveXIDAndActionMappingToTempDir moves the XID and action mapping files to a temporary directory
func moveXIDAndActionMappingToTempDir() (string, string, error) {
	currDir, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("failed to get current directory: %v", err)
	}
	tempDir := filepath.Join(currDir, "test-syslog-monitor")
	sxidFilePath := filepath.Join(tempDir, "sxiderrorsmapping.csv")
	// Create temp directory
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return "", "", fmt.Errorf("failed to create temp directory: %v", err)
	}

	sxidContent := `
10001,NVSWITCH_ERR_HW_HOST_PRIV_ERROR,IGNORE,NONFATAL
10002,NVSWITCH_ERR_HW_HOST_PRIV_TIMEOUT,IGNORE,NONFATAL
10003,NVSWITCH_ERR_HW_HOST_UNHANDLED_INTERRUPT,RESET_FABRIC,FATAL
10004,NVSWITCH_ERR_HW_HOST_THERMAL_EVENT_START,CHECK_THERMALS,FATAL
24007,NVSWITCH_ERR_HW_NPORT_SOURCETRACK_SOURCETRACK_TIME_OUT_ERR,RESET_GPU,FATAL
`

	actionMappingPath := filepath.Join(tempDir, "actionmapping.ini")
	actionFilePathContent := `
	[errorrecommendactiontoplatformconnectormapping]
		IGNORE = 0
		NODE_REBOOT = 1
		COMPONENT_RESET = 2
		COMPONENT_REPLACEMENT = 3
		RESTART_APP = 4
		REPORT_ISSUE = 5
		UNEXPECTED_ERR_REPORT_ISSUE = 5
		RUN_FIELDDIAG = 6
		WORKFLOW_XID_13_31 = 7
		WORKFLOW_ECC_DBE_SRAM = 8
		WORKFLOW_NVLINK5_ERR = 9
		WORKFLOW_XID_45 = 10
		WORKFLOW_XID_48 = 11
		CHECK_MECHANICALS = 12
		WORKFLOW_NVLINK_ERR = 13
		UPDATE_SWFW = 14
		RESTART_VM = 15
		RESET_FABRIC = 16
		WORKFLOW_NVLINK_POTENTIALY_FATAL_ERR = 17
		CHECK_FM_CONFIG = 18
		CHECK_THERMALS = 19
		RESET_GPU = 20
		CHECK_LINK_MECHANICAL_CONNECTIONS = 21
		INVESTIGATE_LINK_SI = 22
		CHECK_UVM = 23
		RESTART_BM = 24
		RESOLUTION_BUCKET_TBD = 99
	`

	if err := os.WriteFile(sxidFilePath, []byte(sxidContent), 0644); err != nil {
		return "", "", fmt.Errorf("failed to write SXID file: %v", err)
	}

	if err := os.WriteFile(actionMappingPath, []byte(actionFilePathContent), 0644); err != nil {
		return "", "", fmt.Errorf("failed to write action mapping file: %v", err)
	}

	return sxidFilePath, actionMappingPath, nil
}
