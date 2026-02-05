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

//go:build !goverter

package v1alpha1

import pb "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"

// converter is the singleton instance of the generated Converter implementation.
var converter Converter = &ConverterImpl{}

// FromProto converts a protobuf Gpu message pointer to a GPU object pointer.
func FromProto(in *pb.Gpu) *GPU {
	if in == nil {
		return nil
	}

	val := converter.FromProtobuf(in)

	return &val
}

// ToProto converts a GPU object pointer to a protobuf Gpu message pointer.
func ToProto(in *GPU) *pb.Gpu {
	if in == nil {
		return nil
	}

	return converter.ToProtobuf(*in)
}

// FromProtoList converts a protobuf GpuList message pointer to a GPUList object pointer.
func FromProtoList(in *pb.GpuList) *GPUList {
	if in == nil {
		return nil
	}

	return converter.FromProtobufList(in)
}

// ToProtoList converts a GPUList object pointer to a protobuf GpuList message pointer.
func ToProtoList(in *GPUList) *pb.GpuList {
	if in == nil {
		return nil
	}

	return converter.ToProtobufList(in)
}
