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

package sxid

import (
	"regexp"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
)

var (
	reSXIDPattern = regexp.MustCompile(
		`nvidia-nvswitch(\d+): SXid \(PCI:([0-9a-fA-F:.]+)\): (\d+), (Fatal|Non-fatal), Link (\d+) (.+)`)
)

type SXIDHandler struct {
	errorResolutionMap    map[int]types.ErrorResolution
	nodeName              string
	defaultAgentName      string
	defaultComponentClass string
	checkName             string
}

type sxidErrorEvent struct {
	ErrorNum  int
	IsFatal   bool
	IsHealthy bool
	NVSwitch  int
	PCI       string
	Link      int
	Message   string
}
