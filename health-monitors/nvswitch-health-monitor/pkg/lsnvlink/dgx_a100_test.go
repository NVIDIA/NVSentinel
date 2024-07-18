/*
* Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package lsnvlink

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestA100NVLink(t *testing.T) {
	testA100ShowGpuNVLink(t)
	testA100ShowNVSwitchNVLink(t)
	testA100GetNVSwitchFromGpuNVLink(t)
	testA100GetGPUFromNVSwitchNVLink(t)
}

func testA100ShowGpuNVLink(t *testing.T) {
	testCases := []struct {
		name string
		gpu  int
		str  string
		err  string
	}{
		{
			name: "Case 0: gpu index 0",
			gpu:  0,
			str: `GPU0:[ 0  1] -------------------- [ 8  9]:NVSWITCH3
GPU0:[ 2  3] -------------------- [24 25]:NVSWITCH0
GPU0:[ 4  5] -------------------- [30 31]:NVSWITCH2
GPU0:[ 6  7] -------------------- [12 13]:NVSWITCH5
GPU0:[ 8  9] -------------------- [12 13]:NVSWITCH1
GPU0:[10 11] -------------------- [30 31]:NVSWITCH4
`,
		},
		{
			name: "Case 1: gpu index 1",
			gpu:  1,
			str: `GPU1:[ 0  1] -------------------- [30 31]:NVSWITCH3
GPU1:[ 2  3] -------------------- [26 27]:NVSWITCH0
GPU1:[ 4  5] -------------------- [12 13]:NVSWITCH2
GPU1:[ 6  7] -------------------- [24 25]:NVSWITCH5
GPU1:[ 8  9] -------------------- [34 35]:NVSWITCH1
GPU1:[10 11] -------------------- [14 15]:NVSWITCH4
`,
		},
		{
			name: "Case 2: gpu index 2",
			gpu:  2,
			str: `GPU2:[ 0  1] -------------------- [28 29]:NVSWITCH3
GPU2:[ 2  3] -------------------- [34 35]:NVSWITCH0
GPU2:[ 4  5] -------------------- [34 35]:NVSWITCH2
GPU2:[ 6  7] -------------------- [26 27]:NVSWITCH5
GPU2:[ 8  9] -------------------- [ 8  9]:NVSWITCH1
GPU2:[10 11] -------------------- [12 13]:NVSWITCH4
`,
		},
		{
			name: "Case 3: gpu index 3",
			gpu:  3,
			str: `GPU3:[ 0  1] -------------------- [34 35]:NVSWITCH3
GPU3:[ 2  3] -------------------- [32 33]:NVSWITCH0
GPU3:[ 4  5] -------------------- [14 15]:NVSWITCH2
GPU3:[ 6  7] -------------------- [14 15]:NVSWITCH5
GPU3:[ 8  9] -------------------- [14 15]:NVSWITCH1
GPU3:[10 11] -------------------- [34 35]:NVSWITCH4
`,
		},
		{
			name: "Case 4: gpu index 4",
			gpu:  4,
			str: `GPU4:[ 0  1] -------------------- [26 27]:NVSWITCH3
GPU4:[ 2  3] -------------------- [10 11]:NVSWITCH0
GPU4:[ 4  5] -------------------- [28 29]:NVSWITCH2
GPU4:[ 6  7] -------------------- [28 29]:NVSWITCH5
GPU4:[ 8  9] -------------------- [10 11]:NVSWITCH1
GPU4:[10 11] -------------------- [26 27]:NVSWITCH4
`,
		},
		{
			name: "Case 5: gpu index 5",
			gpu:  5,
			str: `GPU5:[ 0  1] -------------------- [10 11]:NVSWITCH3
GPU5:[ 2  3] -------------------- [28 29]:NVSWITCH0
GPU5:[ 4  5] -------------------- [ 8  9]:NVSWITCH2
GPU5:[ 6  7] -------------------- [10 11]:NVSWITCH5
GPU5:[ 8  9] -------------------- [24 25]:NVSWITCH1
GPU5:[10 11] -------------------- [28 29]:NVSWITCH4
`,
		},
		{
			name: "Case 6: gpu index 6",
			gpu:  6,
			str: `GPU6:[ 0  1] -------------------- [24 25]:NVSWITCH3
GPU6:[ 2  3] -------------------- [30 31]:NVSWITCH0
GPU6:[ 4  5] -------------------- [26 27]:NVSWITCH2
GPU6:[ 6  7] -------------------- [30 31]:NVSWITCH5
GPU6:[ 8  9] -------------------- [26 27]:NVSWITCH1
GPU6:[10 11] -------------------- [10 11]:NVSWITCH4
`,
		},
		{
			name: "Case 7: gpu index 7",
			gpu:  7,
			str: `GPU7:[ 0  1] -------------------- [32 33]:NVSWITCH3
GPU7:[ 2  3] -------------------- [ 8  9]:NVSWITCH0
GPU7:[ 4  5] -------------------- [10 11]:NVSWITCH2
GPU7:[ 6  7] -------------------- [34 35]:NVSWITCH5
GPU7:[ 8  9] -------------------- [32 33]:NVSWITCH1
GPU7:[10 11] -------------------- [24 25]:NVSWITCH4
`,
		},
		{
			name: "Case 8: wrong gpu index 8",
			gpu:  8,
			err:  "not valid input: Gpu 8",
		},
		{
			name: "Case 9: wrong gpu index -1",
			gpu:  -1,
			err:  "not valid input: Gpu -1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			str, err := DGX_A100{}.ShowGpuNVLink(tc.gpu)
			if len(tc.err) != 0 {
				require.EqualError(t, err, tc.err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.str, str)
			}
		})
	}
}

func testA100ShowNVSwitchNVLink(t *testing.T) {
	testCases := []struct {
		name     string
		nvswitch int
		str      string
		err      string
	}{
		{
			name:     "Case 0: nvswitch index 0",
			nvswitch: 0,
			str: `NVSWITCH0:[24 25] -------------------- [ 2  3]:GPU0
NVSWITCH0:[26 27] -------------------- [ 2  3]:GPU1
NVSWITCH0:[34 35] -------------------- [ 2  3]:GPU2
NVSWITCH0:[32 33] -------------------- [ 2  3]:GPU3
NVSWITCH0:[10 11] -------------------- [ 2  3]:GPU4
NVSWITCH0:[28 29] -------------------- [ 2  3]:GPU5
NVSWITCH0:[30 31] -------------------- [ 2  3]:GPU6
NVSWITCH0:[ 8  9] -------------------- [ 2  3]:GPU7
`,
		},
		{
			name:     "Case 1: nvswitch index 1",
			nvswitch: 1,
			str: `NVSWITCH1:[12 13] -------------------- [ 8  9]:GPU0
NVSWITCH1:[34 35] -------------------- [ 8  9]:GPU1
NVSWITCH1:[ 8  9] -------------------- [ 8  9]:GPU2
NVSWITCH1:[14 15] -------------------- [ 8  9]:GPU3
NVSWITCH1:[10 11] -------------------- [ 8  9]:GPU4
NVSWITCH1:[24 25] -------------------- [ 8  9]:GPU5
NVSWITCH1:[26 27] -------------------- [ 8  9]:GPU6
NVSWITCH1:[32 33] -------------------- [ 8  9]:GPU7
`,
		},
		{
			name:     "Case 2: nvswitch index 2",
			nvswitch: 2,
			str: `NVSWITCH2:[30 31] -------------------- [ 4  5]:GPU0
NVSWITCH2:[12 13] -------------------- [ 4  5]:GPU1
NVSWITCH2:[34 35] -------------------- [ 4  5]:GPU2
NVSWITCH2:[14 15] -------------------- [ 4  5]:GPU3
NVSWITCH2:[28 29] -------------------- [ 4  5]:GPU4
NVSWITCH2:[ 8  9] -------------------- [ 4  5]:GPU5
NVSWITCH2:[26 27] -------------------- [ 4  5]:GPU6
NVSWITCH2:[10 11] -------------------- [ 4  5]:GPU7
`,
		},
		{
			name:     "Case 3: nvswitch index 3",
			nvswitch: 3,
			str: `NVSWITCH3:[ 8  9] -------------------- [ 0  1]:GPU0
NVSWITCH3:[30 31] -------------------- [ 0  1]:GPU1
NVSWITCH3:[28 29] -------------------- [ 0  1]:GPU2
NVSWITCH3:[34 35] -------------------- [ 0  1]:GPU3
NVSWITCH3:[26 27] -------------------- [ 0  1]:GPU4
NVSWITCH3:[10 11] -------------------- [ 0  1]:GPU5
NVSWITCH3:[24 25] -------------------- [ 0  1]:GPU6
NVSWITCH3:[32 33] -------------------- [ 0  1]:GPU7
`,
		},
		{
			name:     "Case 4: nvswitch index 4",
			nvswitch: 4,
			str: `NVSWITCH4:[30 31] -------------------- [10 11]:GPU0
NVSWITCH4:[14 15] -------------------- [10 11]:GPU1
NVSWITCH4:[12 13] -------------------- [10 11]:GPU2
NVSWITCH4:[34 35] -------------------- [10 11]:GPU3
NVSWITCH4:[26 27] -------------------- [10 11]:GPU4
NVSWITCH4:[28 29] -------------------- [10 11]:GPU5
NVSWITCH4:[10 11] -------------------- [10 11]:GPU6
NVSWITCH4:[24 25] -------------------- [10 11]:GPU7
`,
		},
		{
			name:     "Case 5: nvswitch index 5",
			nvswitch: 5,
			str: `NVSWITCH5:[12 13] -------------------- [ 6  7]:GPU0
NVSWITCH5:[24 25] -------------------- [ 6  7]:GPU1
NVSWITCH5:[26 27] -------------------- [ 6  7]:GPU2
NVSWITCH5:[14 15] -------------------- [ 6  7]:GPU3
NVSWITCH5:[28 29] -------------------- [ 6  7]:GPU4
NVSWITCH5:[10 11] -------------------- [ 6  7]:GPU5
NVSWITCH5:[30 31] -------------------- [ 6  7]:GPU6
NVSWITCH5:[34 35] -------------------- [ 6  7]:GPU7
`,
		},
		{
			name:     "Case 6: wrong nvswitch index 6",
			nvswitch: 6,
			err:      "not valid input: NVSwitch 6",
		},
		{
			name:     "Case 7: wrong nvswitch index -1",
			nvswitch: -1,
			err:      "not valid input: NVSwitch -1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			str, err := DGX_A100{}.ShowNVSwitchNVLink(tc.nvswitch)
			if len(tc.err) != 0 {
				require.EqualError(t, err, tc.err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.str, str)
			}
		})
	}
}

func testA100GetNVSwitchFromGpuNVLink(t *testing.T) {
	testCases := []struct {
		gpu      int
		nvlink   int
		nvswitch int
		err      string
	}{
		{gpu: 0, nvlink: 0, nvswitch: 3},
		{gpu: 4, nvlink: 8, nvswitch: 1},
		{gpu: 4, nvlink: 9, nvswitch: 1},
		{gpu: 6, nvlink: 0, nvswitch: 3},
		{gpu: 7, nvlink: 11, nvswitch: 4},
		{gpu: 8, nvlink: 11, nvswitch: -1, err: "not valid input: Gpu 8"},
		{gpu: -1, nvlink: 11, nvswitch: -1, err: "not valid input: Gpu -1"},
		{gpu: 3, nvlink: 12, nvswitch: -1, err: "not valid input: NVLink 12"},
		{gpu: 3, nvlink: -1, nvswitch: -1, err: "not valid input: NVLink -1"},
	}

	for i, tc := range testCases {
		name := fmt.Sprintf("GetNVSwitchFromGpuNVLink/Case %d: gpu %d nvlink %d", i, tc.gpu, tc.nvlink)
		t.Run(name, func(t *testing.T) {
			nvswitch, err := DGX_A100{}.GetNVSwitchFromGpuNVLink(tc.gpu, tc.nvlink)
			if len(tc.err) != 0 {
				require.EqualError(t, err, tc.err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.nvswitch, nvswitch)
			}
		})
	}
}

func testA100GetGPUFromNVSwitchNVLink(t *testing.T) {
	testCases := []struct {
		nvswitch int
		nvlink   int
		gpu      int
		err      string
	}{

		{nvswitch: 0, nvlink: 31, gpu: 6},
		{nvswitch: 0, nvlink: 10, gpu: 4},
		{nvswitch: 4, nvlink: 11, gpu: 6},
		{nvswitch: 5, nvlink: 35, gpu: 7},
		{nvswitch: 0, nvlink: 15, gpu: -1, err: "not valid input: NVSwitch 0, NVLink 15"},
		{nvswitch: 3, nvlink: 0, gpu: -1, err: "not valid input: NVSwitch 3, NVLink 0"},
		{nvswitch: 6, nvlink: 30, gpu: -1, err: "not valid input: NVSwitch 6"},
		{nvswitch: -1, nvlink: 31, gpu: -1, err: "not valid input: NVSwitch -1"},
		{nvswitch: 4, nvlink: -1, gpu: -1, err: "not valid input: NVLink -1"},
	}

	for i, tc := range testCases {
		name := fmt.Sprintf("GetGPUFromNVSwitchNVLink/Case %d nvswitch %d nvlink %d", i, tc.nvswitch, tc.nvlink)
		t.Run(name, func(t *testing.T) {
			gpu, err := DGX_A100{}.GetGpuFromNVSwitchNVLink(tc.nvswitch, tc.nvlink)
			if len(tc.err) != 0 {
				require.Equal(t, tc.gpu, gpu)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.gpu, gpu)
			}
		})
	}
}
