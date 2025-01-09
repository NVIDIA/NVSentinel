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

package lsnvlink

import (
	"fmt"
	"strings"
)

var (
	// [GPU_ID][NVSWITCH_ID]map[GPU_NVLINK_ID1][NVSWITCH_NVLINK_ID2]
	h100_gpu_nvswitch_nvlinks = [][][][]int{
		{ // GPU 0
			{{2, 3, 12, 13}, {40, 41, 44, 45}},         // NVSwitch 0
			{{0, 1, 11, 16, 17}, {36, 37, 40, 46, 47}}, // NVSwitch 1
			{{15, 14, 10, 6, 7}, {42, 43, 45, 62, 63}}, // NVSwitch 2
			{{4, 5, 9, 8}, {58, 59, 62, 63}},           // // NVSwitch 3
		},
		{ // GPU 1
			{{15, 14, 8, 9}, {42, 43, 46, 47}},       // NVSwitch 0
			{{2, 3, 7, 6, 11}, {2, 3, 4, 5, 32}},     // NVSwitch 1
			{{10, 5, 4, 0, 1}, {34, 40, 41, 46, 47}}, // NVSwitch 2
			{{12, 13, 16, 17}, {34, 35, 38, 39}},     // NVSwitch 3
		},
		{ // GPU 2
			{{13, 12, 7, 6}, {48, 49, 52, 53}},         // NVSwitch 0
			{{17, 16, 10, 3, 2}, {0, 1, 33, 38, 39}},   // NVSwitch 1
			{{14, 15, 8, 9, 11}, {16, 17, 50, 51, 52}}, // NVSwitch 2
			{{5, 4, 1, 0}, {56, 57, 60, 61}},           // NVSwitch 3
		},
		{ // GPU 3
			{{9, 8, 13, 12}, {32, 33, 36, 37}},         // NVSwitch 0
			{{2, 3, 10, 14, 15}, {50, 51, 53, 62, 63}}, // NVSwitch 1
			{{7, 6, 11, 16, 17}, {2, 3, 35, 38, 39}},   // NVSwitch 2
			{{5, 4, 1, 0}, {42, 43, 46, 47}},           // NVSwitch 3
		},
		{ // GPU 4
			{{7, 6, 12, 13}, {58, 59, 62, 63}},         // NVSwitch 0
			{{17, 16, 11, 1, 0}, {48, 49, 52, 56, 57}}, // NVSwitch 1
			{{15, 14, 10, 2, 3}, {36, 37, 44, 60, 61}}, // NVSwitch 2
			{{4, 5, 9, 8}, {48, 49, 52, 53}},           // NVSwitch 3
		},
		{ // GPU 5
			{{6, 7, 15, 14}, {34, 35, 38, 39}},       // NVSwitch 0
			{{8, 9, 17, 16, 11}, {6, 7, 34, 35, 42}}, // NVSwitch 1
			{{4, 5, 10, 1, 0}, {0, 1, 19, 32, 33}},   // NVSwitch 2
			{{13, 12, 3, 2}, {32, 33, 36, 37}},       // NVSwitch 3
		},
		{ // GPU 6

			{{17, 16, 13, 12}, {50, 51, 54, 55}},       // NVSwitch 0
			{{10, 0, 1, 4, 5}, {43, 54, 55, 58, 59}},   // NVSwitch 1
			{{15, 14, 11, 8, 9}, {48, 49, 53, 56, 57}}, // NVSwitch 2
			{{7, 6, 3, 2}, {40, 41, 44, 45}},           // NVSwitch 3
		},
		{ // GPU 7
			{{12, 13, 17, 16}, {56, 57, 60, 61}},       // NVSwitch 0
			{{10, 5, 4, 0, 1}, {41, 44, 45, 60, 61}},   // NVSwitch 1
			{{11, 14, 15, 7, 6}, {18, 54, 55, 58, 59}}, // NVSwitch 2
			{{2, 3, 8, 9}, {50, 51, 54, 55}},           // NVSwitch 3
		},
	}

	// [GPU_ID]map[NVLINK_ID]NVSWITCH_ID
	h100_gpu_map_nvlink_nvswitch = func() []map[int]int {
		ret := make([]map[int]int, H100_GPU_COUNT)

		for gpu, nvswitch_nvlinks := range h100_gpu_nvswitch_nvlinks {
			ret[gpu] = map[int]int{}
			for nvswitch, nvlinks := range nvswitch_nvlinks {
				for _, nvlink := range nvlinks[0] {
					ret[gpu][nvlink] = nvswitch
				}
			}
		}

		return ret
	}()

	// [NVSWITCH_ID]map[NVLINK_ID]GPU_ID
	h100_nvswitch_map_nvlink_gpu = func() []map[int]int {
		ret := make([]map[int]int, H100_GPU_COUNT)

		for gpu, nvswitch_nvlinks := range h100_gpu_nvswitch_nvlinks {
			for nvswitch, nvlinks := range nvswitch_nvlinks {
				if ret[nvswitch] == nil {
					ret[nvswitch] = map[int]int{}
				}

				for _, nvlink := range nvlinks[1] {
					ret[nvswitch][nvlink] = gpu
				}
			}
		}

		return ret
	}()
)

type DGX_H100 struct{}

func (DGX_H100) ShowGpuNVLink(gpu int) (string, error) {
	var ret string

	if gpu < 0 || gpu >= H100_GPU_COUNT {
		return ret, fmt.Errorf("not valid input: Gpu %d", gpu)
	}

	nvswitch_nvlinks := h100_gpu_nvswitch_nvlinks[gpu]

	for nvswitch, nvlinks := range nvswitch_nvlinks {
		for i := range len(nvlinks[0]) {
			ret += fmt.Sprintf("GPU%d:[%2d] %s [%2d]:NVSWITCH%d\n",
				gpu,
				nvlinks[0][i],
				strings.Repeat("-", 20),
				nvlinks[1][i],
				nvswitch,
			)
		}
	}

	return ret, nil
}

func (DGX_H100) ShowNVSwitchNVLink(nvswitch int) (string, error) {
	var ret string

	if nvswitch < 0 || nvswitch >= H100_NVSWITCH_COUNT {
		return ret, fmt.Errorf("not valid input: NVSwitch %d", nvswitch)
	}

	for gpu, nvswitch_nvlinks := range h100_gpu_nvswitch_nvlinks {
		nvlinks := nvswitch_nvlinks[nvswitch]
		for i := range len(nvlinks[0]) {
			ret += fmt.Sprintf("NVSWITCH%d:[%2d] %s [%2d]:GPU%d\n",
				nvswitch,
				nvlinks[1][i],
				strings.Repeat("-", 20),
				nvlinks[0][i],
				gpu,
			)
		}
	}

	return ret, nil
}

func (DGX_H100) GetNVSwitchFromGpuNVLink(gpu, nvlink int) (int, error) {
	if gpu < 0 || gpu >= H100_GPU_COUNT {
		return -1, fmt.Errorf("not valid input: Gpu %d", gpu)
	}

	if nvlink < 0 {
		return -1, fmt.Errorf("not valid input: NVLink %d", nvlink)
	}

	map_nvlink_nvswitch := h100_gpu_map_nvlink_nvswitch[gpu]

	if nvswitch, ok := map_nvlink_nvswitch[nvlink]; ok {
		return nvswitch, nil
	}

	return -1, fmt.Errorf("not valid input: Gpu %d, NVLink %d", gpu, nvlink)
}

func (DGX_H100) GetGpuFromNVSwitchNVLink(nvswitch, nvlink int) (int, error) {
	if nvswitch < 0 || nvswitch >= H100_NVSWITCH_COUNT {
		return -1, fmt.Errorf("not valid input: NVSwitch %d", nvswitch)
	}

	if nvlink < 0 {
		return -1, fmt.Errorf("not valid input: NVLink %d", nvlink)
	}

	map_nvlink_gpu := h100_nvswitch_map_nvlink_gpu[nvswitch]

	if gpu, ok := map_nvlink_gpu[nvlink]; ok {
		return gpu, nil
	}

	return -1, fmt.Errorf("not valid input: NVSwitch %d, NVLink %d", nvswitch, nvlink)
}

func (DGX_H100) GetAllGPUIds() []int {
	return []int{0, 1, 2, 3, 4, 5, 6, 7}
}

func (DGX_H100) GetAllNVSwitchIds() []int {
	return []int{0, 1, 2, 3}
}

func (DGX_H100) GetAllNVLinkIds() []int {
	return []int{
		// 0 to 19
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9,
		10, 11, 12, 13, 14, 15, 16, 17, 18, 19,

		// 32 to 63
		32, 33, 34, 35, 36, 37, 38, 39,
		40, 41, 42, 43, 44, 45, 46, 47,
		48, 49, 50, 51, 52, 53, 54, 55,
		56, 57, 58, 59, 60, 61, 62, 63,
	}
}
