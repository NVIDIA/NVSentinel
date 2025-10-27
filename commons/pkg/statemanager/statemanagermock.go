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

package statemanager

import (
	"context"
)

// MockStateManager provides an implementation of the StateManager interface. This mock implementation is leverage by
// unit tests in both the node-drainer-module and fault-remediation-module.
type MockStateManager struct {
	UpdateNVSentinelStateNodeLabelFn func(ctx context.Context, nodeName string,
		newStateLabelValue NVSentinelStateLabelValue, removeStateLabel bool) (bool, error)
}

func (manager *MockStateManager) UpdateNVSentinelStateNodeLabel(ctx context.Context, nodeName string,
	newStateLabelValue NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
	return manager.UpdateNVSentinelStateNodeLabelFn(ctx, nodeName, newStateLabelValue, removeStateLabel)
}
