// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package event

import (
	"fmt"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// Normalizer defines the interface for transforming raw CSP events
// into the standardized MaintenanceEvent model.
type Normalizer interface {
	// Normalize attempts to convert the raw event data into a MaintenanceEvent.
	// The rawEvent is expected to be the specific type for the implementing CSP.
	// additionalInfo can be used to pass context like nodeName, instanceID, entityArn for AWS.
	Normalize(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error)
}

// GetNormalizer is a factory function that returns the appropriate normalizer
// based on the provided CSP type string (e.g., model.CSPGCP).
func GetNormalizer(csp model.CSP) (Normalizer, error) {
	switch csp {
	case model.CSPGCP:
		return &GCPNormalizer{}, nil // GCPNormalizer is defined in gcp_normalizer.go
	case model.CSPAWS:
		return &AWSNormalizer{}, nil // AWSNormalizer is defined in aws_normalizer.go
	default:
		return nil, fmt.Errorf("no normalizer available for CSP: %s", csp)
	}
}
