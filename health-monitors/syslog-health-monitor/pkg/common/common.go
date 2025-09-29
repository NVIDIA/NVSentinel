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
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
	"gopkg.in/ini.v1"
	"k8s.io/klog/v2"
)

func LoadActionMappings(actionMappingFilePath string) error {
	cfg, err := ini.Load(actionMappingFilePath)
	if err != nil {
		klog.Errorf("Action mapping INI file not found at %s; no action mappings loaded", actionMappingFilePath)
		return fmt.Errorf("action mapping INI file not found at %s: %w", actionMappingFilePath, err)
	}

	section := cfg.Section(types.ActionMappingSection)
	if section == nil {
		klog.Errorf("Section '%s' not found in INI file", types.ActionMappingSection)
		return fmt.Errorf("section '%s' not found in INI file", types.ActionMappingSection)
	}

	for _, key := range section.Keys() {
		actionName := key.Name()

		codeValue, err := key.Int()
		if err != nil {
			klog.Warningf("Invalid integer value for action '%s': %v", actionName, err)
			continue
		}

		types.ActionMap[actionName] = codeValue
	}

	klog.Infof("Loaded %d action mappings from INI file", len(types.ActionMap))

	return nil
}

func MapActionStringToProto(s string) (pb.RecommenedAction, bool) {
	s = strings.TrimSpace(strings.ToUpper(s))
	//nolint:gosec // G115: integer overflow not applicable
	if code, ok := types.ActionMap[s]; ok {
		return pb.RecommenedAction(code), true
	}

	return pb.RecommenedAction_REPORT_ISSUE, false
}

func LoadErrorResolutionMap(errorMappingPath string) (map[int]types.ErrorResolution, error) {
	errorResolutionMap := make(map[int]types.ErrorResolution)

	f, err := os.Open(errorMappingPath)
	if err != nil {
		klog.Errorf("error mapping file not found at %s; defaulting to REPORT_ISSUE", errorMappingPath)
		return errorResolutionMap, fmt.Errorf("error mapping file not found at %s", errorMappingPath)
	}

	defer func() {
		if cerr := f.Close(); cerr != nil {
			klog.Errorf("Error closing Xid mapping file: %v", cerr)
		}
	}()

	reader := csv.NewReader(f)
	reader.Comment = '#'
	reader.FieldsPerRecord = 4 // Always expect exactly 4 fields: XID code, description, recommended action, fatality

	for {
		errRes := types.ErrorResolution{}

		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			klog.Errorf("Error reading CSV record: %s", err.Error())
			continue
		}

		codeStr := strings.TrimSpace(record[0])
		actionStr := strings.TrimSpace(record[2])

		code, err := strconv.Atoi(codeStr)
		if err != nil {
			klog.Errorf("Error parsing XID code %s: %s", codeStr, err.Error())
			continue
		}

		action, ok := MapActionStringToProto(actionStr)
		if ok {
			errRes.RecommendedAction = action
		}

		errorResolutionMap[code] = types.ErrorResolution(errRes)
	}

	return errorResolutionMap, nil
}
