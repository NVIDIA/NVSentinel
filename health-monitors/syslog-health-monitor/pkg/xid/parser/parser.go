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

// Parser defines the interface for XID parsing implementations
type Parser interface {
	// Parse attempts to parse XID information from a message
	Parse(message string) (*Response, error)
}

// Request represents a request to parse XID information
type Request struct {
	XIDMessage    string `json:"xid_message"`
	DriverVersion string `json:"driver_version"`
}

// Response represents the result of XID parsing
type Response struct {
	Success bool       `json:"success"`
	Result  XIDDetails `json:"result"`
	Error   string     `json:"error,omitempty"`
}

// XIDDetails contains the parsed XID information
type XIDDetails struct {
	Context             string            `json:"context"`
	DecodedXIDStr       string            `json:"decoded_xid_string"`
	Driver              string            `json:"driver"`
	InvestigatoryAction string            `json:"investigatory_action"`
	Machine             string            `json:"machine"`
	Mnemonic            string            `json:"mnemonic"`
	Name                string            `json:"name"`
	Number              int               `json:"number"`
	PCIE                string            `json:"pcie_bdf"`
	Resolution          string            `json:"resolution"`
	Metadata            map[string]string `json:"metadata"`
}
