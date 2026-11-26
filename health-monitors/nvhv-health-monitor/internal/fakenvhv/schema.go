// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package fakenvhv

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	_ "google.golang.org/protobuf/types/known/structpb" // registers google/protobuf/struct.proto, which the schema imports
)

// Package and service names of the schema the fake serves.
const (
	Package     = "health.verdict.v1"
	ServiceName = Package + ".VerdictService"
)

type (
	fieldType = descriptorpb.FieldDescriptorProto_Type
	label     = descriptorpb.FieldDescriptorProto_Label
)

// FileDescriptorProto is the verdict schema the fake serves: the messages, enums
// and service the monitor reads, with the field numbers NVHV uses. It returns a
// new copy each time, so a test can change it to play an NVHV whose schema
// changed. This is a small stand-in written here, not a copy of NVHV's proto.
func FileDescriptorProto() *descriptorpb.FileDescriptorProto {
	const (
		str  = descriptorpb.FieldDescriptorProto_TYPE_STRING
		bl   = descriptorpb.FieldDescriptorProto_TYPE_BOOL
		msg  = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		enum = descriptorpb.FieldDescriptorProto_TYPE_ENUM

		one  = descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		many = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	)

	field := func(name string, number int32, t fieldType, l label, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(number), Type: t.Enum(), Label: l.Enum(),
		}
		if typeName != "" {
			f.TypeName = proto.String("." + Package + "." + typeName)
		}

		return f
	}

	enumOf := func(name string, values ...string) *descriptorpb.EnumDescriptorProto {
		e := &descriptorpb.EnumDescriptorProto{Name: proto.String(name)}
		for i, v := range values {
			e.Value = append(e.Value, &descriptorpb.EnumValueDescriptorProto{Name: proto.String(v), Number: proto.Int32(int32(i))})
		}

		return e
	}

	observed := field("observed_values", 16, msg, many, "CheckVerdict.ObservedValuesEntry")

	value := field("value", 2, msg, one, "")
	value.TypeName = proto.String(".google.protobuf.Value")

	return &descriptorpb.FileDescriptorProto{
		Name:       proto.String("health/v1/verdict.proto"),
		Package:    proto.String(Package),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/protobuf/struct.proto"},
		EnumType: []*descriptorpb.EnumDescriptorProto{
			enumOf("Severity", "SEVERITY_UNSPECIFIED", "PASS", "INFO", "WARNING", "UNKNOWN", "CRITICAL"),
			enumOf("Category", "gpu", "nvlink", "pcie", "infiniband", "storage", "node", "xid", "cpu"),
			enumOf("OperationalState", "OPERATIONAL_STATE_UNSPECIFIED", "ready", "monitor", "drain"),
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("EvaluateRequest")},
			{
				Name: proto.String("NodeVerdict"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("schema_version", 1, str, one, ""),
					field("check_results", 10, msg, many, "CheckVerdict"),
				},
			},
			{
				Name: proto.String("CheckVerdict"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("check_id", 2, str, one, ""),
					field("severity", 5, enum, one, "Severity"),
					field("category", 6, enum, one, "Category"),
					field("skipped", 9, bl, one, ""),
					field("messages", 13, str, many, ""),
					field("recommended_action", 14, msg, one, "RecommendedAction"),
					field("affected_entities", 15, str, many, ""),
					observed,
				},
				NestedType: []*descriptorpb.DescriptorProto{{
					Name:    proto.String("ObservedValuesEntry"),
					Field:   []*descriptorpb.FieldDescriptorProto{field("key", 1, str, one, ""), value},
					Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
				}},
			},
			{
				Name: proto.String("RecommendedAction"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("operational_state", 1, enum, one, "OperationalState"),
					field("remediation_actions", 2, str, many, ""),
				},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("VerdictService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String("Evaluate"),
				InputType:  proto.String("." + Package + ".EvaluateRequest"),
				OutputType: proto.String("." + Package + ".NodeVerdict"),
			}},
		}},
	}
}

// files builds the registry of the served schema, with the well-known file it
// imports.
func files(fdp *descriptorpb.FileDescriptorProto) *protoregistry.Files {
	f := &protoregistry.Files{}

	structFile, err := protoregistry.GlobalFiles.FindFileByPath("google/protobuf/struct.proto")
	if err != nil {
		panic(err)
	}

	if err := f.RegisterFile(structFile); err != nil {
		panic(err)
	}

	fd, err := protodesc.NewFile(fdp, f)
	if err != nil {
		panic(err)
	}

	if err := f.RegisterFile(fd); err != nil {
		panic(err)
	}

	return f
}
