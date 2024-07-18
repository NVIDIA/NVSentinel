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

func TestH100NVLink(t *testing.T) {
	testH100ShowGpuNVLink(t)
	testH100ShowNVSwitchNVLink(t)
	testH100GetNVSwitchFromGpuNVLink(t)
	testH100GetGPUFromNVSwitchNVLink(t)
}

func testH100ShowGpuNVLink(t *testing.T) {
	testCases := []struct {
		name string
		gpu  int
		str  string
		err  string
	}{
		{
			name: "Case 0: gpu index 0",
			gpu:  0,
			str: `GPU0:[ 2] -------------------- [40]:NVSWITCH0
GPU0:[ 3] -------------------- [41]:NVSWITCH0
GPU0:[12] -------------------- [44]:NVSWITCH0
GPU0:[13] -------------------- [45]:NVSWITCH0
GPU0:[ 0] -------------------- [36]:NVSWITCH1
GPU0:[ 1] -------------------- [37]:NVSWITCH1
GPU0:[11] -------------------- [40]:NVSWITCH1
GPU0:[16] -------------------- [46]:NVSWITCH1
GPU0:[17] -------------------- [47]:NVSWITCH1
GPU0:[15] -------------------- [42]:NVSWITCH2
GPU0:[14] -------------------- [43]:NVSWITCH2
GPU0:[10] -------------------- [45]:NVSWITCH2
GPU0:[ 6] -------------------- [62]:NVSWITCH2
GPU0:[ 7] -------------------- [63]:NVSWITCH2
GPU0:[ 4] -------------------- [58]:NVSWITCH3
GPU0:[ 5] -------------------- [59]:NVSWITCH3
GPU0:[ 9] -------------------- [62]:NVSWITCH3
GPU0:[ 8] -------------------- [63]:NVSWITCH3
`,
		},
		{
			name: "Case 1: gpu index 1",
			gpu:  1,
		},
		{
			name: "Case 2: gpu index 2",
			gpu:  2,
		},
		{
			name: "Case 3: gpu index 3",
			gpu:  3,
		},
		{
			name: "Case 4: gpu index 4",
			gpu:  4,
		},
		{
			name: "Case 5: gpu index 5",
			gpu:  5,
		},
		{
			name: "Case 6: gpu index 6",
			gpu:  6,
		},
		{
			name: "Case 7: gpu index 7",
			gpu:  7,
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
			str, err := DGX_H100{}.ShowGpuNVLink(tc.gpu)
			if len(tc.err) != 0 {
				require.EqualError(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}

			if len(tc.str) != 0 {
				require.Equal(t, tc.str, str)
			}
		})
	}
}

func testH100ShowNVSwitchNVLink(t *testing.T) {
	testCases := []struct {
		name     string
		nvswitch int
		str      string
		err      string
	}{
		{
			name:     "Case 0: nvswitch index 0",
			nvswitch: 0,
		},
		{
			name:     "Case 1: nvswitch index 1",
			nvswitch: 1,
		},
		{
			name:     "Case 2: nvswitch index 2",
			nvswitch: 2,
		},
		{
			name:     "Case 3: nvswitch index 3",
			nvswitch: 3,
			str: `NVSWITCH3:[58] -------------------- [ 4]:GPU0
NVSWITCH3:[59] -------------------- [ 5]:GPU0
NVSWITCH3:[62] -------------------- [ 9]:GPU0
NVSWITCH3:[63] -------------------- [ 8]:GPU0
NVSWITCH3:[34] -------------------- [12]:GPU1
NVSWITCH3:[35] -------------------- [13]:GPU1
NVSWITCH3:[38] -------------------- [16]:GPU1
NVSWITCH3:[39] -------------------- [17]:GPU1
NVSWITCH3:[56] -------------------- [ 5]:GPU2
NVSWITCH3:[57] -------------------- [ 4]:GPU2
NVSWITCH3:[60] -------------------- [ 1]:GPU2
NVSWITCH3:[61] -------------------- [ 0]:GPU2
NVSWITCH3:[42] -------------------- [ 5]:GPU3
NVSWITCH3:[43] -------------------- [ 4]:GPU3
NVSWITCH3:[46] -------------------- [ 1]:GPU3
NVSWITCH3:[47] -------------------- [ 0]:GPU3
NVSWITCH3:[48] -------------------- [ 4]:GPU4
NVSWITCH3:[49] -------------------- [ 5]:GPU4
NVSWITCH3:[52] -------------------- [ 9]:GPU4
NVSWITCH3:[53] -------------------- [ 8]:GPU4
NVSWITCH3:[32] -------------------- [13]:GPU5
NVSWITCH3:[33] -------------------- [12]:GPU5
NVSWITCH3:[36] -------------------- [ 3]:GPU5
NVSWITCH3:[37] -------------------- [ 2]:GPU5
NVSWITCH3:[40] -------------------- [ 7]:GPU6
NVSWITCH3:[41] -------------------- [ 6]:GPU6
NVSWITCH3:[44] -------------------- [ 3]:GPU6
NVSWITCH3:[45] -------------------- [ 2]:GPU6
NVSWITCH3:[50] -------------------- [ 2]:GPU7
NVSWITCH3:[51] -------------------- [ 3]:GPU7
NVSWITCH3:[54] -------------------- [ 8]:GPU7
NVSWITCH3:[55] -------------------- [ 9]:GPU7
`,
		},
		{
			name:     "Case 4: nvswitch index 4",
			nvswitch: 4,
			err:      "not valid input: NVSwitch 4",
		},
		{
			name:     "Case 5: wrong nvswitch index -1",
			nvswitch: -1,
			err:      "not valid input: NVSwitch -1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			str, err := DGX_H100{}.ShowNVSwitchNVLink(tc.nvswitch)
			if len(tc.err) != 0 {
				require.EqualError(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			if len(tc.str) != 0 {
				require.Equal(t, tc.str, str)
			}
		})
	}
}

func testH100GetNVSwitchFromGpuNVLink(t *testing.T) {
	testCases := []struct {
		gpu      int
		nvlink   int
		nvswitch int
		err      string
	}{
		{gpu: 0, nvlink: 0, nvswitch: 1},
		{gpu: 2, nvlink: 12, nvswitch: 0},
		{gpu: 4, nvlink: 17, nvswitch: 1},
		{gpu: 5, nvlink: 10, nvswitch: 2},
		{gpu: 7, nvlink: 15, nvswitch: 2},
		{gpu: 8, nvlink: 11, nvswitch: -1, err: "not valid input: Gpu 8"},
		{gpu: -1, nvlink: 11, nvswitch: -1, err: "not valid input: Gpu -1"},
		{gpu: 3, nvlink: 18, nvswitch: -1, err: "not valid input: Gpu 3, NVLink 18"},
		{gpu: 3, nvlink: -1, nvswitch: -1, err: "not valid input: NVLink -1"},
	}

	for i, tc := range testCases {
		name := fmt.Sprintf("GetNVSwitchFromGpuNVLink/Case %d: gpu %d nvlink %d", i, tc.gpu, tc.nvlink)
		t.Run(name, func(t *testing.T) {
			nvswitch, err := DGX_H100{}.GetNVSwitchFromGpuNVLink(tc.gpu, tc.nvlink)
			if len(tc.err) != 0 {
				require.EqualError(t, err, tc.err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.nvswitch, nvswitch)
			}
		})
	}
}

func testH100GetGPUFromNVSwitchNVLink(t *testing.T) {
	testCases := []struct {
		nvswitch int
		nvlink   int
		gpu      int
		err      string
	}{
		{nvswitch: 0, nvlink: 49, gpu: 2},
		{nvswitch: 1, nvlink: 57, gpu: 4},
		{nvswitch: 2, nvlink: 40, gpu: 1},
		{nvswitch: 3, nvlink: 51, gpu: 7},
		{nvswitch: 0, nvlink: 15, gpu: -1, err: "not valid input: NVSwitch 0, NVLink 15"},
		{nvswitch: 3, nvlink: 0, gpu: -1, err: "not valid input: NVSwitch 3, NVLink 0"},
		{nvswitch: 6, nvlink: 30, gpu: -1, err: "not valid input: NVSwitch 6"},
		{nvswitch: -1, nvlink: 31, gpu: -1, err: "not valid input: NVSwitch -1"},
		{nvswitch: 4, nvlink: -1, gpu: -1, err: "not valid input: NVLink -1"},
	}

	for i, tc := range testCases {
		name := fmt.Sprintf("GetGPUFromNVSwitchNVLink/Case %d nvswitch %d nvlink %d", i, tc.nvswitch, tc.nvlink)
		t.Run(name, func(t *testing.T) {
			gpu, err := DGX_H100{}.GetGpuFromNVSwitchNVLink(tc.nvswitch, tc.nvlink)
			if len(tc.err) != 0 {
				require.Equal(t, tc.gpu, gpu)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.gpu, gpu)
			}
		})
	}
}
