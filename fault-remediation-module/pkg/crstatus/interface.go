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

package crstatus

import (
	"context"
)

// Common constants for all maintenance CRs
const (
	MaintenanceAPIGroup   = "janitor.dgxc.nvidia.com"
	MaintenanceAPIVersion = "v1alpha1"
)

// CRStatus represents the status of a maintenance CR
type CRStatus string

const (
	// CRStatusSucceeded indicates the CR has completed successfully
	CRStatusSucceeded CRStatus = "Succeeded"
	// CRStatusInProgress indicates the CR is still being processed
	CRStatusInProgress CRStatus = "InProgress"
	// CRStatusFailed indicates the CR has failed
	CRStatusFailed CRStatus = "Failed"
	// CRStatusNotFound indicates the CR was not found
	CRStatusNotFound CRStatus = "NotFound"
)

// CRStatusChecker abstracts the checking of maintenance CR status
type CRStatusChecker interface {
	GetCRStatus(ctx context.Context, crName string) (CRStatus, error)
}
