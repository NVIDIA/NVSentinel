// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMockStateManagerUpdateNVSentinelStateNodeLabelIfMatch_NilFunction_ReturnsNoChange(t *testing.T) {
	manager := &MockStateManager{}

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		context.Background(),
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
}

func TestMockStateManagerUpdateNVSentinelStateNodeLabelIfMatch_ConfiguredFunction_DelegatesArguments(t *testing.T) {
	ctx := context.Background()
	expectedErr := errors.New("configured error")
	called := false
	manager := &MockStateManager{
		UpdateNVSentinelStateNodeLabelIfMatchFn: func(
			actualCtx context.Context,
			nodeName string,
			newValue NVSentinelStateLabelValue,
			expectedValues ...NVSentinelStateLabelValue,
		) (bool, error) {
			called = true
			assert.Equal(t, ctx, actualCtx)
			assert.Equal(t, testNodeName, nodeName)
			assert.Equal(t, DrainStoppedLabelValue, newValue)
			assert.Equal(t, []NVSentinelStateLabelValue{
				QuarantinedLabelValue,
				DrainStoppedLabelValue,
			}, expectedValues)

			return true, expectedErr
		},
	}

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
		DrainStoppedLabelValue,
	)

	assert.True(t, called)
	assert.True(t, nodeModified)
	assert.ErrorIs(t, err, expectedErr)
}
