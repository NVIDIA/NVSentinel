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

package mapper

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMinorNumberResolver(t *testing.T) {
	uuids := map[int]string{0: "GPU-a0", 7: "GPU-a7", 10: "GPU-a10"}
	resolve := newMinorNumberResolver(func() (map[int]string, error) { return uuids, nil })

	for name, want := range map[string]string{"gpu-0": "GPU-a0", "gpu-7": "GPU-a7", "gpu-10": "GPU-a10"} {
		uuid, ok := resolve(name)
		assert.True(t, ok, name)
		assert.Equal(t, want, uuid, name)
	}

	// Anything but the exact name the driver derives from a known minor fails closed.
	for _, name := range []string{"gpu-1", "gpu-07", "gpu--1", "gpu-+7", "gpu-", "gpu-0-mig-1g.10gb", "GPU-7", "7"} {
		_, ok := resolve(name)
		assert.False(t, ok, name)
	}

	failing := newMinorNumberResolver(func() (map[int]string, error) { return nil, errors.New("nvml down") })
	_, ok := failing("gpu-0")
	assert.False(t, ok)
}
