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

package xid

import (
	"regexp"

	"github.com/hashicorp/go-retryablehttp"
)

var (
	reNvrmMap = regexp.MustCompile(`NVRM: GPU at PCI:([0-9a-fA-F:.]+): (GPU-[0-9a-fA-F-]+)`)
)

type XIDHandler struct {
	nodeName              string
	defaultAgentName      string
	defaultComponentClass string
	checkName             string

	pciToGPUUUID map[string]string // Runtime map of PCI ID -> GPU UUID
	url          string
	client       *retryablehttp.Client
}

type Request struct {
	XIDMessage string `json:"xid_message"`
}

type Response struct {
	Success bool       `json:"success"`
	Result  XIDDetails `json:"result,omitempty"`
	Error   string     `json:"error,omitempty"`
}

type XIDDetails struct {
	Context             string `json:"context"`
	DecodedXIDStr       string `json:"decoded_xid_string"`
	Driver              string `json:"driver"`
	InvestigatoryAction string `json:"investigatory_action"`
	Machine             string `json:"machine"`
	Mnemonic            string `json:"mnemonic"`
	Name                string `json:"name"`
	Number              int    `json:"number"`
	PCIE                string `json:"pcie_bdf"`
	Resolution          string `json:"resolution"`
}
