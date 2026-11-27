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

package nvhvclient

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/internal/mocknvhv"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

func ctx(t *testing.T) context.Context {
	t.Helper()

	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	return c
}

func sampleVerdict() *verdict.Verdict {
	return &verdict.Verdict{
		SchemaVersion: "0.6",
		Results: []verdict.Result{{
			CheckID:            "row_remap",
			Severity:           verdict.Critical,
			Category:           "gpu",
			Messages:           []string{"remap failed", "second line"},
			AffectedEntities:   []string{"GPU-1"},
			OperationalState:   "drain",
			RemediationActions: []string{"reset_gpu", "service_required"},
			ObservedStrings:    map[string]string{"pci": "0000:1b:00.0"},
		}, {
			CheckID:  "xid",
			Severity: verdict.Unknown,
			Category: "xid",
		}},
	}
}

func TestEvaluateReadsTheVerdictThroughReflection(t *testing.T) {
	srv, addr := mocknvhv.Start(t, mocknvhv.Options{})
	srv.Set(sampleVerdict(), nil)

	got, err := New(dial(t, addr)).Evaluate(ctx(t), verdict.Request{})
	require.NoError(t, err)

	assert.Equal(t, sampleVerdict(), got)
}

func TestSchemaIsReadOnceAndAgainAfterAFailedCall(t *testing.T) {
	srv, addr := mocknvhv.Start(t, mocknvhv.Options{})
	srv.Set(sampleVerdict(), nil)
	c := New(dial(t, addr))

	for range 3 {
		_, err := c.Evaluate(ctx(t), verdict.Request{})
		require.NoError(t, err)
	}

	assert.Equal(t, 1, srv.ReflectionCalls(), "the schema is cached")

	srv.Set(nil, status.Error(codes.Internal, "boom"))

	_, err := c.Evaluate(ctx(t), verdict.Request{})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err), "the gRPC status survives the wrapping")

	srv.Set(sampleVerdict(), nil)

	_, err = c.Evaluate(ctx(t), verdict.Request{})
	require.NoError(t, err)
	assert.Equal(t, 2, srv.ReflectionCalls(), "a failed call makes the next one read the schema again")
}

// filesWith returns the mock's schema with edit applied to its file proto.
func filesWith(t *testing.T, edit func(*descriptorpb.FileDescriptorProto)) *protoregistry.Files {
	t.Helper()

	fdp := mocknvhv.FileDescriptorProto()
	edit(fdp)

	files, err := buildFiles(map[string]*descriptorpb.FileDescriptorProto{fdp.GetName(): fdp})
	require.NoError(t, err)

	return files
}

func message(fdp *descriptorpb.FileDescriptorProto, name string) *descriptorpb.DescriptorProto {
	for _, m := range fdp.GetMessageType() {
		if m.GetName() == name {
			return m
		}
	}

	return nil
}

func TestNewSchemaRejectsAChangeThatBreaksTheMonitor(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*descriptorpb.FileDescriptorProto)
		want string
	}{
		"a renamed field": {
			func(f *descriptorpb.FileDescriptorProto) {
				for _, fd := range message(f, "CheckVerdict").Field {
					if fd.GetName() == "check_id" {
						fd.Name, fd.JsonName = proto.String("check_identifier"), proto.String("checkIdentifier")
					}
				}
			},
			"CheckVerdict.check_id",
		},
		"a renamed window field": {
			func(f *descriptorpb.FileDescriptorProto) {
				for _, fd := range message(f, "EvaluateRequest").Field {
					if fd.GetName() == "xid_from" {
						fd.Name = proto.String("xid_start")
					}
				}
			},
			"EvaluateRequest.xid_from",
		},
		"a field of another type": {
			func(f *descriptorpb.FileDescriptorProto) {
				for _, fd := range message(f, "CheckVerdict").Field {
					if fd.GetName() == "skipped" {
						fd.Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
					}
				}
			},
			"CheckVerdict.skipped",
		},
		"a field that stopped being repeated": {
			func(f *descriptorpb.FileDescriptorProto) {
				for _, fd := range message(f, "CheckVerdict").Field {
					if fd.GetName() == "messages" {
						fd.Label = descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
					}
				}
			},
			"CheckVerdict.messages",
		},
		"a severity that was removed": {
			func(f *descriptorpb.FileDescriptorProto) {
				for _, e := range f.EnumType {
					if e.GetName() == "Severity" {
						for i, v := range e.Value {
							if v.GetName() == "CRITICAL" {
								e.Value = append(e.Value[:i], e.Value[i+1:]...)

								break
							}
						}
					}
				}
			},
			"CRITICAL",
		},
		"no Evaluate method": {
			func(f *descriptorpb.FileDescriptorProto) { f.Service[0].Method = nil },
			"Evaluate",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newSchema(filesWith(t, tc.edit))
			require.ErrorIs(t, err, ErrIncompatibleSchema)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestNewFieldsInNVHVAreIgnored(t *testing.T) {
	_, err := newSchema(filesWith(t, func(f *descriptorpb.FileDescriptorProto) {
		m := message(f, "CheckVerdict")
		m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
			Name:     proto.String("something_new"),
			Number:   proto.Int32(99),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			JsonName: proto.String("somethingNew"),
		})
	}))
	require.NoError(t, err)
}

func TestEvaluateSendsTheXidWindow(t *testing.T) {
	srv, addr := mocknvhv.Start(t, mocknvhv.Options{})
	c := New(dial(t, addr))

	from, to := int64(1000), int64(1015)

	_, err := c.Evaluate(ctx(t), verdict.Request{XidFrom: &from, XidTo: &to})
	require.NoError(t, err)

	got := srv.LastRequest()
	require.NotNil(t, got.XidFrom)
	require.NotNil(t, got.XidTo)
	assert.Equal(t, from, *got.XidFrom)
	assert.Equal(t, to, *got.XidTo)

	// No window asked for: the fields stay unset, so NVHV reads unbounded.
	_, err = c.Evaluate(ctx(t), verdict.Request{})
	require.NoError(t, err)

	got = srv.LastRequest()
	assert.Nil(t, got.XidFrom)
	assert.Nil(t, got.XidTo)
}

func TestNewSchemaRejectsXidBoundsInARealOneof(t *testing.T) {
	// In a real oneof, setting xid_to would clear xid_from.
	_, err := newSchema(filesWith(t, func(f *descriptorpb.FileDescriptorProto) {
		m := message(f, "EvaluateRequest")
		m.OneofDecl = []*descriptorpb.OneofDescriptorProto{{Name: proto.String("bounds")}}

		for _, fd := range m.Field {
			fd.Proto3Optional = nil
			fd.OneofIndex = proto.Int32(0)
		}
	}))
	require.ErrorIs(t, err, ErrIncompatibleSchema)
	assert.ErrorContains(t, err, "must not be in a oneof")
}

// withValueMessage points observed_values at a message of its own whose
// string_value, if given, has the type t.
func withValueMessage(f *descriptorpb.FileDescriptorProto, t *descriptorpb.FieldDescriptorProto_Type) {
	value := &descriptorpb.DescriptorProto{Name: proto.String("MyValue")}
	if t != nil {
		value.Field = []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("string_value"), Number: proto.Int32(1), Type: t.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}}
	}

	f.MessageType = append(f.MessageType, value)

	for _, fd := range message(f, "CheckVerdict").NestedType[0].Field {
		if fd.GetName() == "value" {
			fd.TypeName = proto.String(".health.verdict.v1.MyValue")
		}
	}
}

func TestNewSchemaChecksTheTypeOfStringValue(t *testing.T) {
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	num := descriptorpb.FieldDescriptorProto_TYPE_INT64

	_, err := newSchema(filesWith(t, func(f *descriptorpb.FileDescriptorProto) { withValueMessage(f, &str) }))
	require.NoError(t, err, "a string string_value is fine")

	_, err = newSchema(filesWith(t, func(f *descriptorpb.FileDescriptorProto) { withValueMessage(f, nil) }))
	require.NoError(t, err, "no string_value just means no observed strings")

	// An int64 string_value would otherwise be read as text, as "123".
	_, err = newSchema(filesWith(t, func(f *descriptorpb.FileDescriptorProto) { withValueMessage(f, &num) }))
	require.ErrorIs(t, err, ErrIncompatibleSchema)
	assert.ErrorContains(t, err, "string_value must be a string")
}

func TestErrorsCarryContextAndKeepTheirCause(t *testing.T) {
	_, addr := mocknvhv.Start(t, mocknvhv.Options{NoReflection: true})

	_, err := New(dial(t, addr)).Evaluate(ctx(t), verdict.Request{})
	require.Error(t, err)
	assert.ErrorContains(t, err, "failed to load the NVHV verdict schema: failed to fetch the verdict schema from NVHV through gRPC reflection:")
	assert.Equal(t, codes.Unimplemented, status.Code(err), "the gRPC status survives the wrapping")

	schema := mocknvhv.FileDescriptorProto()
	message(schema, "CheckVerdict").Field[0].Name = proto.String("check_identifier")

	_, addr = mocknvhv.Start(t, mocknvhv.Options{Schema: schema})

	_, err = New(dial(t, addr)).Evaluate(ctx(t), verdict.Request{})
	require.ErrorIs(t, err, ErrIncompatibleSchema, "the sentinel is still found through the wrapping")
	assert.ErrorContains(t, err, "failed to read the verdict schema that NVHV serves: failed to bind the verdict fields the monitor reads:")
}
