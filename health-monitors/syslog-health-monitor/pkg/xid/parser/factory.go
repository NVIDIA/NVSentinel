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

package parser

import (
	"fmt"

	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"
	"k8s.io/klog/v2"
)

// ParserConfig holds configuration for parser creation
type ParserConfig struct {
	NodeName            string
	XidAnalyserEndpoint string
	SidecarEnabled      bool
}

// CreateParser creates the appropriate parser based on configuration
func CreateParser(config ParserConfig) (Parser, error) {
	if config.SidecarEnabled {
		if config.XidAnalyserEndpoint == "" {
			return nil, fmt.Errorf("XidAnalyserEndpoint is required when SidecarEnabled is true")
		}

		klog.Infof("Creating sidecar parser with endpoint: %s", config.XidAnalyserEndpoint)

		return NewSidecarParser(config.XidAnalyserEndpoint, config.NodeName), nil
	}

	klog.Infof("Creating Excel parser with embedded mapping file")

	errorResolutionMap, err := common.LoadErrorResolutionMap()
	if err != nil {
		return nil, fmt.Errorf("failed to load XID error resolution map from embedded Excel file: %w", err)
	}

	nvl5Rules, err := common.GetNVL5DecodingRules()
	if err != nil {
		return nil, fmt.Errorf("failed to load NVL5 decoding rules from embedded Excel file: %w", err)
	}

	klog.Infof("Loaded %d XID error resolution mappings and %d NVL5 rule types", len(errorResolutionMap), len(nvl5Rules))

	return NewCSVParser(config.NodeName, errorResolutionMap, nvl5Rules), nil
}
