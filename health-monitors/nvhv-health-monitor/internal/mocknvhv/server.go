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

// Package mocknvhv is a mock NVHV server for tests. It serves a configurable
// verdict over gRPC and, like NVHV, its schema through gRPC reflection, so the
// monitor's client can be tested without a real NVHV. The schema is a small one
// written in this package, and a test can serve a changed one with StartWith.
package mocknvhv

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

// Server serves the verdict set with Set.
type Server struct {
	response protoreflect.MessageDescriptor

	mu      sync.Mutex
	v       *verdict.Verdict
	err     error
	lastReq verdict.Request

	reflectionCalls atomic.Int32
}

// Options change what the mock serves.
type Options struct {
	// NoReflection turns reflection off, as in an NVHV that does not have it.
	NoReflection bool
	// Schema is the schema to serve instead of FileDescriptorProto().
	Schema *descriptorpb.FileDescriptorProto
}

// Start runs a Server on a loopback port until the test ends and returns it with
// its address.
func Start(t testing.TB, opts Options) (*Server, string) {
	t.Helper()

	if opts.Schema == nil {
		opts.Schema = FileDescriptorProto()
	}

	reg := files(opts.Schema)

	d, err := reg.FindDescriptorByName(ServiceName)
	if err != nil {
		t.Fatal(err)
	}

	svc := d.(protoreflect.ServiceDescriptor) //nolint:forcetypeassert // the schema is ours
	method := svc.Methods().ByName("Evaluate")

	s := &Server{response: method.Output(), v: &verdict.Verdict{}}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	g := grpc.NewServer(grpc.StreamInterceptor(func(
		srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler,
	) error {
		if info.IsServerStream && info.IsClientStream {
			s.reflectionCalls.Add(1)
		}

		return h(srv, ss)
	}))

	g.RegisterService(&grpc.ServiceDesc{
		ServiceName: string(svc.FullName()),
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: string(method.Name()),
			Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				req := dynamicpb.NewMessage(method.Input())
				if err := dec(req); err != nil {
					return nil, err
				}

				return s.evaluate(ctx, readRequest(req))
			},
		}},
	}, s)

	if !opts.NoReflection {
		reflectionv1.RegisterServerReflectionServer(g, reflection.NewServerV1(reflection.ServerOptions{
			Services:           g,
			DescriptorResolver: reg,
			ExtensionResolver:  protoregistry.GlobalTypes, // the schema has no extensions
		}))
	}

	go func() { _ = g.Serve(lis) }()

	t.Cleanup(g.Stop)

	return s, lis.Addr().String()
}

// Set makes the server return v, or fail with err if err is set.
func (s *Server) Set(v *verdict.Verdict, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.v, s.err = v, err
}

// ReflectionCalls is how many reflection streams clients have opened.
func (s *Server) ReflectionCalls() int { return int(s.reflectionCalls.Load()) }

// LastRequest is what the latest Evaluate call asked for.
func (s *Server) LastRequest() verdict.Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastReq
}

func (s *Server) evaluate(_ context.Context, r verdict.Request) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastReq = r

	if s.err != nil {
		return nil, s.err
	}

	return encode(s.response, s.v), nil
}

// readRequest reads the Xid window of an Evaluate request.
func readRequest(m *dynamicpb.Message) verdict.Request {
	var r verdict.Request

	fields := m.Descriptor().Fields()
	if fd := fields.ByName("xid_from"); fd != nil && m.Has(fd) {
		v := m.Get(fd).Int()
		r.XidFrom = &v
	}

	if fd := fields.ByName("xid_to"); fd != nil && m.Has(fd) {
		v := m.Get(fd).Int()
		r.XidTo = &v
	}

	return r
}

// encode writes a verdict into a dynamic message of the served schema.
func encode(md protoreflect.MessageDescriptor, v *verdict.Verdict) *dynamicpb.Message {
	m := dynamicpb.NewMessage(md)
	fields := md.Fields()

	m.Set(fields.ByName("schema_version"), protoreflect.ValueOfString(v.SchemaVersion))

	results := m.Mutable(fields.ByName("check_results")).List()
	for _, r := range v.Results {
		results.Append(protoreflect.ValueOfMessage(encodeResult(fields.ByName("check_results").Message(), r)))
	}

	return m
}

func encodeResult(md protoreflect.MessageDescriptor, r verdict.Result) protoreflect.Message {
	m := dynamicpb.NewMessage(md)
	f := md.Fields()

	m.Set(f.ByName("check_id"), protoreflect.ValueOfString(r.CheckID))
	m.Set(f.ByName("severity"), enumValue(f.ByName("severity"), string(r.Severity)))
	m.Set(f.ByName("category"), enumValue(f.ByName("category"), r.Category))
	m.Set(f.ByName("skipped"), protoreflect.ValueOfBool(r.Skipped))
	appendStrings(m, f.ByName("messages"), r.Messages)
	appendStrings(m, f.ByName("affected_entities"), r.AffectedEntities)

	if r.OperationalState != "" || len(r.RemediationActions) > 0 {
		am := dynamicpb.NewMessage(f.ByName("recommended_action").Message())
		af := am.Descriptor().Fields()
		am.Set(af.ByName("operational_state"), enumValue(af.ByName("operational_state"), r.OperationalState))
		appendStrings(am, af.ByName("remediation_actions"), r.RemediationActions)
		m.Set(f.ByName("recommended_action"), protoreflect.ValueOfMessage(am))
	}

	observed := m.Mutable(f.ByName("observed_values")).Map()
	valueDesc := f.ByName("observed_values").MapValue().Message()

	for k, s := range r.ObservedStrings {
		vm := dynamicpb.NewMessage(valueDesc)
		vm.Set(valueDesc.Fields().ByName("string_value"), protoreflect.ValueOfString(s))
		observed.Set(protoreflect.ValueOfString(k).MapKey(), protoreflect.ValueOfMessage(vm))
	}

	return m
}

func enumValue(fd protoreflect.FieldDescriptor, name string) protoreflect.Value {
	if v := fd.Enum().Values().ByName(protoreflect.Name(name)); v != nil {
		return protoreflect.ValueOfEnum(v.Number())
	}

	return protoreflect.ValueOfEnum(0)
}

func appendStrings(m *dynamicpb.Message, fd protoreflect.FieldDescriptor, values []string) {
	l := m.Mutable(fd).List()
	for _, v := range values {
		l.Append(protoreflect.ValueOfString(v))
	}
}
