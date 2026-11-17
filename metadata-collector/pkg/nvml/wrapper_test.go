// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nvml

import (
	"encoding/binary"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	nvmlmock "github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"github.com/stretchr/testify/require"
)

func TestNvlinkPresentLinkCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     uint32
		outerRet  nvml.Return
		fieldRet  nvml.Return
		valueType nvml.ValueType
		expected  *uint64
	}{
		{
			name:      "zero on L40",
			value:     0,
			outerRet:  nvml.SUCCESS,
			fieldRet:  nvml.SUCCESS,
			valueType: nvml.VALUE_TYPE_UNSIGNED_INT,
			expected:  uint64Ptr(0),
		},
		{
			name:      "nonzero on H100",
			value:     18,
			outerRet:  nvml.SUCCESS,
			fieldRet:  nvml.SUCCESS,
			valueType: nvml.VALUE_TYPE_UNSIGNED_INT,
			expected:  uint64Ptr(18),
		},
		{
			name:      "outer call fails",
			value:     0,
			outerRet:  nvml.ERROR_NOT_SUPPORTED,
			fieldRet:  nvml.SUCCESS,
			valueType: nvml.VALUE_TYPE_UNSIGNED_INT,
			expected:  nil,
		},
		{
			name:      "field read fails",
			value:     0,
			outerRet:  nvml.SUCCESS,
			fieldRet:  nvml.ERROR_NOT_SUPPORTED,
			valueType: nvml.VALUE_TYPE_UNSIGNED_INT,
			expected:  nil,
		},
		{
			name:      "unexpected value type",
			value:     0,
			outerRet:  nvml.SUCCESS,
			fieldRet:  nvml.SUCCESS,
			valueType: nvml.VALUE_TYPE_DOUBLE,
			expected:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			device := &nvmlmock.Device{
				GetFieldValuesFunc: func(fields []nvml.FieldValue) nvml.Return {
					require.Len(t, fields, 1)
					require.Equal(t, uint32(nvml.FI_DEV_NVLINK_LINK_COUNT), fields[0].FieldId)

					binary.NativeEndian.PutUint32(fields[0].Value[:4], tt.value)
					fields[0].NvmlReturn = uint32(tt.fieldRet)
					fields[0].ValueType = uint32(tt.valueType)

					return tt.outerRet
				},
			}

			require.Equal(t, tt.expected, nvlinkPresentLinkCount(device))
		})
	}
}

func uint64Ptr(value uint64) *uint64 {
	return &value
}
