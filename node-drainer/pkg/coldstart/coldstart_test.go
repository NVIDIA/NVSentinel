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

package coldstart

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

func TestIsActiveQuarantineStatus(t *testing.T) {
	assert.True(t, isActiveQuarantineStatus(string(model.Quarantined)))
	assert.True(t, isActiveQuarantineStatus(string(model.AlreadyQuarantined)))
	assert.False(t, isActiveQuarantineStatus(string(model.UnQuarantined)))
	assert.False(t, isActiveQuarantineStatus(string(model.Cancelled)))
	assert.False(t, isActiveQuarantineStatus(string(model.StatusNotStarted)))
	assert.False(t, isActiveQuarantineStatus(""))
}

func TestColdStartQuery_WithCutoff(t *testing.T) {
	cutoff := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	filter := coldStartQuery(cutoff).ToMongo()
	assert.Equal(t, map[string]interface{}{"$gt": cutoff}, filter["createdAt"])
	assert.Contains(t, filter, "$or")
}
