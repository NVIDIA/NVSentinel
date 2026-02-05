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

package v1alpha1

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	pb "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var lastTransitionTime = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

func TestGPUConversion_Nil(t *testing.T) {
	if out := FromProto(nil); out != nil {
		t.Errorf("FromProto(nil): got %#v, expected nil", out)
	}
	if out := ToProto(nil); out != nil {
		t.Errorf("ToProto(nil): got %#v, expected nil", out)
	}
	if out := FromProtoList(nil); out != nil {
		t.Errorf("FromProtoList(nil): got %#v, expected nil", out)
	}
	if out := ToProtoList(nil); out != nil {
		t.Errorf("ToProtoList(nil): got %#v, expected nil", out)
	}
}

func TestGPUConversion(t *testing.T) {

	protoIn := &pb.Gpu{
		Metadata: &pb.ObjectMeta{
			Name:            "gpu-1111",
			ResourceVersion: "1",
		},
		Spec: &pb.GpuSpec{
			Uuid: "GPU-1111",
		},
		Status: &pb.GpuStatus{
			Conditions: []*pb.Condition{
				{
					Type:               "Ready",
					Status:             "False",
					LastTransitionTime: timestamppb.New(lastTransitionTime),
					Reason:             "DriverCrash",
					Message:            "The driver has stopped responding.",
				},
			},
			RecommendedAction: "ResetGPU",
		},
	}

	goStruct := FromProto(protoIn)

	expectedName := strings.ToLower(protoIn.Metadata.Name)
	if goStruct.ObjectMeta.Name != expectedName {
		t.Errorf("ObjectMeta.Name conversion failed: got %q, want %q",
			goStruct.ObjectMeta.Name, expectedName)
	}

	expectedResourceVersion := "1"
	if goStruct.ObjectMeta.ResourceVersion != expectedResourceVersion {
		t.Errorf("ObjectMeta.ResourceVersion conversion failed: got %q, want %q",
			goStruct.ObjectMeta.ResourceVersion, expectedResourceVersion)
	}

	protoOut := ToProto(goStruct)

	if diff := cmp.Diff(protoIn, protoOut, protocmp.Transform()); diff != "" {
		t.Errorf("Conversion failed (-want +got):\n%s", diff)
	}
}

func TestGPUListConversion(t *testing.T) {
	protoIn := &pb.GpuList{
		Metadata: &pb.ListMeta{
			ResourceVersion: "2",
		},
		Items: []*pb.Gpu{
			{
				Metadata: &pb.ObjectMeta{
					Name:            "gpu-1111",
					ResourceVersion: "1",
				},
				Spec: &pb.GpuSpec{
					Uuid: "GPU-1111",
				},
				Status: &pb.GpuStatus{
					Conditions: []*pb.Condition{
						{
							Type:               "Ready",
							Status:             "True",
							LastTransitionTime: timestamppb.New(lastTransitionTime),
							Reason:             "DriverReady",
							Message:            "Driver is posting ready status.",
						},
					},
				},
			},
			{
				Metadata: &pb.ObjectMeta{
					Name:            "gpu-2222",
					ResourceVersion: "2",
				},
				Spec: &pb.GpuSpec{
					Uuid: "GPU-2222",
				},
				Status: &pb.GpuStatus{
					Conditions: []*pb.Condition{
						{
							Type:               "HardwareFailure",
							Status:             "True",
							LastTransitionTime: timestamppb.New(lastTransitionTime.Add(1 * time.Minute)),
							Reason:             "DoubleBitECCError",
							Message:            "Double Bit ECC error detected.",
						},
					},
					RecommendedAction: "RebootNode",
				},
			},
		},
	}

	goList := FromProtoList(protoIn)

	expectedResourceVersion := "2"
	if goList.ListMeta.ResourceVersion != expectedResourceVersion {
		t.Errorf("ListMeta.ResourceVersion conversion failed: got %q, want %q",
			goList.ListMeta.ResourceVersion, expectedResourceVersion)
	}

	protoOut := ToProtoList(goList)

	if diff := cmp.Diff(protoIn, protoOut, protocmp.Transform()); diff != "" {
		t.Errorf("Conversion failed (-want +got):\n%s", diff)
	}
}
