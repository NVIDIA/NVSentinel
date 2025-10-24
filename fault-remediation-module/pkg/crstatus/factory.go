// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"

	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/common"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// CRStatusCheckerFactory creates the appropriate status checker based on recommended action
type CRStatusCheckerFactory struct {
	dynamicClient dynamic.Interface
	restMapper    *restmapper.DeferredDiscoveryRESTMapper
	dryRun        bool
}

// NewCRStatusCheckerFactory creates a new factory for CR status checkers
func NewCRStatusCheckerFactory(
	dynamicClient dynamic.Interface,
	restMapper *restmapper.DeferredDiscoveryRESTMapper,
	dryRun bool,
) *CRStatusCheckerFactory {
	return &CRStatusCheckerFactory{
		dynamicClient: dynamicClient,
		restMapper:    restMapper,
		dryRun:        dryRun,
	}
}

// GetStatusChecker returns the appropriate status checker for the given recommended action
func (f *CRStatusCheckerFactory) GetStatusChecker(action platformconnector.RecommenedAction) (CRStatusChecker, error) {
	// Determine which equivalence group this action belongs to
	group := common.GetRemediationGroupForAction(action)

	// Map group to CR type and return appropriate status checker
	switch group {
	case "restart":
		// All restart-related actions use RebootNode CR
		return NewRebootNodeCRStatusChecker(
			f.dynamicClient,
			f.restMapper,
			f.dryRun,
		), nil

	// Add more CR types here as they are implemented

	default:
		return nil, fmt.Errorf("no status checker available for action: %s (group: %s)", action.String(), group)
	}
}
