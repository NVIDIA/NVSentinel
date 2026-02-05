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
	pb "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Converter is the interface used to generate type conversion methods between the
// Kubernetes Resource Model structs and the Protobuf message structs.
//
// goverter:converter
// goverter:output:file ./zz_generated.goverter.go
// goverter:extend FromProtobufTypeMeta FromProtobufListTypeMeta FromProtobufTimestamp ToProtobufTimestamp
// goverter:useZeroValueOnPointerInconsistency
type Converter interface {
	// FromProtobuf converts a protobuf Gpu message into a GPU object.
	//
	// goverter:map . TypeMeta | FromProtobufTypeMeta
	// goverter:map Metadata ObjectMeta
	FromProtobuf(source *pb.Gpu) GPU

	// ToProtobuf converts a GPU object into a protobuf Gpu message.
	//
	// goverter:map ObjectMeta Metadata
	// goverter:ignore state sizeCache unknownFields ResourceVersion
	ToProtobuf(source GPU) *pb.Gpu

	// FromProtobufList converts a protobuf GpuList message into a GPUList object.
	//
	// goverter:map . TypeMeta | FromProtobufListTypeMeta
	// goverter:map Metadata ListMeta
	FromProtobufList(source *pb.GpuList) *GPUList

	// ToProtobufList converts a GPUList object into a protobuf GpuList message.
	//
	// goverter:map ListMeta Metadata
	// goverter:ignore state sizeCache unknownFields
	ToProtobufList(source *GPUList) *pb.GpuList

	// FromProtobufObjectMeta converts a protobuf ObjectMeta into a metav1.ObjectMeta object.
	//
	// goverter:ignore GenerateName UID Generation CreationTimestamp DeletionTimestamp DeletionGracePeriodSeconds
	// goverter:ignore Labels Annotations OwnerReferences Finalizers ManagedFields SelfLink
	FromProtobufObjectMeta(source *pb.ObjectMeta) metav1.ObjectMeta

	// ToProtobufObjectMeta converts a metav1.ObjectMeta into a protobuf Object message.
	//
	// goverter:ignore state sizeCache unknownFields
	ToProtobufObjectMeta(source metav1.ObjectMeta) *pb.ObjectMeta

	// FromProtobufListMeta converts a protobuf ListMeta into a metav1.ListMeta object.
	//
	// goverter:ignore SelfLink Continue RemainingItemCount
	FromProtobufListMeta(source *pb.ListMeta) metav1.ListMeta

	// ToProtobufListMeta converts a metav1.ListMeta into a protobuf ListMeta message.
	//
	// goverter:ignore state sizeCache unknownFields
	ToProtobufListMeta(source metav1.ListMeta) *pb.ListMeta

	// FromProtobufSpec converts a protobuf GpuSpec message into a GPUSpec object.
	//
	// goverter:map Uuid UUID
	FromProtobufSpec(source *pb.GpuSpec) GPUSpec

	// ToProtobufSpec converts a GPUSpec object into a protobuf GpuSpec message.
	//
	// goverter:map UUID Uuid
	// goverter:ignore state sizeCache unknownFields
	ToProtobufSpec(source GPUSpec) *pb.GpuSpec

	// FromProtobufStatus converts a protobuf GpuStatus message into a GPUStatus object.
	FromProtobufStatus(source *pb.GpuStatus) GPUStatus

	// ToProtobufStatus converts a GPUStatus object into a protobuf GpuStatus message.
	//
	// goverter:ignore state sizeCache unknownFields
	ToProtobufStatus(source GPUStatus) *pb.GpuStatus

	// FromProtobufCondition converts a protobuf Condition message into a metav1.Condition object.
	//
	// goverter:ignore ObservedGeneration
	// Note: ObservedGeneration is specific to k8s and not found in the protobuf Condition message.
	FromProtobufCondition(source *pb.Condition) metav1.Condition

	// ToProtobufCondition converts a metav1.Condition object into a protobuf Condition message.
	//
	// goverter:ignore state sizeCache unknownFields
	ToProtobufCondition(source metav1.Condition) *pb.Condition
}

// FromProtobufTypeMeta generates the standard TypeMeta for the root GPU resource.
func FromProtobufTypeMeta(_ *pb.Gpu) metav1.TypeMeta {
	return metav1.TypeMeta{
		Kind:       "GPU",
		APIVersion: SchemeGroupVersion.String(),
	}
}

// FromProtobufListTypeMeta generates the standard TypeMeta for the GPUList resource.
func FromProtobufListTypeMeta(_ *pb.GpuList) metav1.TypeMeta {
	return metav1.TypeMeta{
		Kind:       "GPUList",
		APIVersion: SchemeGroupVersion.String(),
	}
}

// FromProtobufTimestamp converts a protobuf Timestamp message to a metav1.Time.
func FromProtobufTimestamp(source *timestamppb.Timestamp) metav1.Time {
	if source == nil {
		return metav1.Time{}
	}

	return metav1.NewTime(source.AsTime())
}

// ToProtobufTimestamp converts a metav1.Time to a protobuf Timestamp message.
func ToProtobufTimestamp(source metav1.Time) *timestamppb.Timestamp {
	if source.IsZero() {
		return nil
	}

	return timestamppb.New(source.Time)
}
