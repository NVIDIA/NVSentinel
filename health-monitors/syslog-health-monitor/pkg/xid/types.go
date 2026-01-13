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

package xid

import (
	"regexp"

	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/metadata"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/parser"
)

var (
	reNvrmMap   = regexp.MustCompile(`NVRM: GPU at PCI:([0-9a-fA-F:.]+): (GPU-[0-9a-fA-F-]+)`)
	gpuResetMap = regexp.MustCompile(`GPU reset executed: (GPU-[0-9a-fA-F-]+)`)
)

type XIDHandler struct {
	nodeName              string
	defaultAgentName      string
	defaultComponentClass string
	checkName             string

	pciToGPUUUID   map[string]string
	parser         parser.Parser
	metadataReader *metadata.Reader
}
