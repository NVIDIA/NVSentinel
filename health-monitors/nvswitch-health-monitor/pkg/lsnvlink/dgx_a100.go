package lsnvlink

import (
	"fmt"
	"strings"
)

var (
	a100_nvswitch = []int{3, 0, 2, 5, 1, 4}

	// [NVSWITCH_ID][GPU_ID][NVLINK_ID1, NVLINK_ID2]
	a100_nvswitch_gpu_nvlink = [][][]int{
		{{24, 25}, {26, 27}, {34, 35}, {32, 33}, {10, 11}, {28, 29}, {30, 31}, {8, 9}},
		{{12, 13}, {34, 35}, {8, 9}, {14, 15}, {10, 11}, {24, 25}, {26, 27}, {32, 33}},
		{{30, 31}, {12, 13}, {34, 35}, {14, 15}, {28, 29}, {8, 9}, {26, 27}, {10, 11}},
		{{8, 9}, {30, 31}, {28, 29}, {34, 35}, {26, 27}, {10, 11}, {24, 25}, {32, 33}},
		{{30, 31}, {14, 15}, {12, 13}, {34, 35}, {26, 27}, {28, 29}, {10, 11}, {24, 25}},
		{{12, 13}, {24, 25}, {26, 27}, {14, 15}, {28, 29}, {10, 11}, {30, 31}, {34, 35}},
	}

	// [NVSWITCH_ID]map[NVLINK_ID]GPU_ID
	a100_nvswitch_map_nvlink_gpu = func() []map[int]int {
		ret := make([]map[int]int, A100_NVSWITCH_COUNT)

		for nvswitch, gpu_nvlink := range a100_nvswitch_gpu_nvlink {
			ret[nvswitch] = map[int]int{}

			for gpu, nvlink := range gpu_nvlink {
				ret[nvswitch][nvlink[0]] = gpu
				ret[nvswitch][nvlink[1]] = gpu
			}
		}

		return ret
	}()
)

type DGX_A100 struct{}

func (DGX_A100) ShowGpuNVLink(gpu int) (string, error) {
	var ret string

	if gpu < 0 || gpu >= A100_GPU_COUNT {
		return ret, fmt.Errorf("not valid input: Gpu %d", gpu)
	}

	for i, nvswitch := range a100_nvswitch {
		gpu_nvlink := a100_nvswitch_gpu_nvlink[nvswitch]
		ret += fmt.Sprintf("GPU%d:[%2d %2d] %s %2d:NVSWITCH%d\n",
			gpu,
			i*2, i*2+1,
			strings.Repeat("-", 20),
			gpu_nvlink[gpu],
			nvswitch,
		)
	}

	return ret, nil
}

func getNVswitchIdx(nvswitch int) int {
	for i := range A100_NVSWITCH_COUNT {
		if a100_nvswitch[i] == nvswitch {
			return i
		}
	}
	return -1
}

func (DGX_A100) ShowNVSwitchNVLink(nvswitch int) (string, error) {
	var ret string

	if nvswitch < 0 || nvswitch >= A100_NVSWITCH_COUNT {
		return ret, fmt.Errorf("not valid input: NVSwitch %d", nvswitch)
	}

	gpu_nvlink := a100_nvswitch_gpu_nvlink[nvswitch]
	for gpu := range A100_GPU_COUNT {
		nwswitch_nvlink := gpu_nvlink[gpu]
		nvswitchIdx := getNVswitchIdx(nvswitch)
		ret += fmt.Sprintf("NVSWITCH%d:%2d %s [%2d %2d]:GPU%d\n",
			nvswitch,
			nwswitch_nvlink,
			strings.Repeat("-", 20),
			nvswitchIdx*2, nvswitchIdx*2+1,
			gpu,
		)
	}

	return ret, nil
}

func (DGX_A100) GetNVSwitchFromGpuNVLink(gpu, nvlink int) (int, error) {
	if gpu < 0 || gpu >= A100_GPU_COUNT {
		return -1, fmt.Errorf("not valid input: Gpu %d", gpu)
	}

	if nvlink < 0 || nvlink/2 >= len(a100_nvswitch) {
		return -1, fmt.Errorf("not valid input: NVLink %d", nvlink)
	}

	return a100_nvswitch[nvlink/2], nil
}

func (DGX_A100) GetGpuFromNVSwitchNVLink(nvswitch, nvlink int) (int, error) {
	if nvswitch < 0 || nvswitch >= A100_NVSWITCH_COUNT {
		return -1, fmt.Errorf("not valid input: NVSwitch %d", nvswitch)
	}

	if nvlink < 0 {
		return -1, fmt.Errorf("not valid input: NVLink %d", nvlink)
	}

	if gpu, ok := a100_nvswitch_map_nvlink_gpu[nvswitch][nvlink]; ok {
		return gpu, nil
	} else {
		return -1, fmt.Errorf("not valid input: NVSwitch %d, NVLink %d", nvswitch, nvlink)
	}
}
